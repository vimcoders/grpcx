/*
Copyright The containerd Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/
package transport

import (
	"cmp"
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Option configures the transport Options.
type Option func(*Options)

// WithMaxStreams caps how many concurrent streams a connection may carry.
// Zero falls back to defaultMaxStreams.
func WithMaxStreams(n uint32) Option {
	return func(t *Options) {
		t.maxStreams = cmp.Or(n, uint32(defaultMaxStreams))
	}
}

// WithTimeout sets Options.timeout, the value Options.Timeout reports. Only
// Ping reads it, capping its budget at Keepalive(); a client stream ends at
// WithReadTimeout or the caller's deadline, and the server clamps a handler at
// WithServerTimeout. The header timeout comes from the caller's context
// deadline. Zero falls back to defaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(t *Options) {
		t.timeout = cmp.Or(d, defaultTimeout)
	}
}

// WithReadTimeout sets how long a channel waits for the next frame before
// giving up on the connection. Both sides use it: ServerOptions.Handle passes
// it to every accepted connection's channel, and the client's own channel
// reads replies under it. Zero falls back to defaultReadTimeout.
func WithReadTimeout(d time.Duration) Option {
	return func(so *Options) {
		so.readTimeout = cmp.Or(d, defaultReadTimeout)
	}
}

// WithWriteTimeout sets how long a write may take before failing the call.
// Both sides use it: ServerOptions.Handle passes it to every accepted
// connection's channel, and the client's own channel writes requests under
// it. Zero falls back to defaultWriteTimeout.
func WithWriteTimeout(d time.Duration) Option {
	return func(so *Options) {
		so.writeTimeout = cmp.Or(d, defaultWriteTimeout)
	}
}

// WithCodec sets the codec for request and reply bodies. The call header is
// always encoded with the built-in codec in package encoding, so both peers
// agree on the header whatever this option holds.
func WithCodec(c encoding.Codec) Option {
	return func(t *Options) {
		t.codec = c
	}
}

// WithBufSize sets the send buffer in blocks of defaultBufSize, capped at
// defaultMaxBufSize. Zero disables batching, which is the package default.
//
// With batching on, Send queues the caller's slices by reference until the
// flush, so a payload given to RoundTrip must not change between Send returning
// nil and that flush. With batching off, the bytes are already written when
// Send returns. The flush interval is not configurable; the timer runs at
// defaultFlushInterval.
func WithBufSize(n int) Option {
	return func(c *Options) {
		c.bufsize = min(n*defaultBufSize, defaultMaxBufSize)
	}
}

// Options holds the transport configuration shared by the client and the
// server sides.
type Options struct {
	maxStreams   uint32
	timeout      time.Duration
	readTimeout  time.Duration
	writeTimeout time.Duration
	keepalive    time.Duration
	codec        encoding.Codec
	bufsize      int
}

// Keepalive returns the period at which an idle connection is pinged. It is
// the fixed defaultKeepalive for every connection, not derived from a caller's
// timeout.
func (o Options) Keepalive() time.Duration {
	return o.keepalive
}

// Timeout returns the per-request timeout set by WithTimeout. Only Ping reads
// it, and it caps the value at Keepalive().
func (o Options) Timeout() time.Duration {
	return o.timeout
}

// DefaultOptions returns the transport configuration with every field set to
// its package default.
func DefaultOptions() Options {
	return Options{
		timeout:      defaultTimeout,
		maxStreams:   defaultMaxStreams,
		codec:        encoding.GetCodec(encoding.Name),
		readTimeout:  defaultReadTimeout,
		writeTimeout: defaultWriteTimeout,
		keepalive:    defaultKeepalive,
	}
}

// client is the RoundTripper implementation: it owns the connection, the
// stream table and the receive loop that demultiplexes responses.
type client struct {
	Options
	RoundTripper
	SendCloser
	streams  map[uint32]*clientStream
	streamID uint32
	sync.Once
	sync.RWMutex
}

// Dial connects to endpoint over TCP with the background context.
func Dial(endpoint string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

// DialContext connects to endpoint over TCP, applies opts to both the
// transport configuration and the connection, then starts the receive loop.
// The dial itself runs under defaultTimeout, not WithTimeout: that option
// only reaches Ping, so a slow dial is bounded by the package default.
func DialContext(ctx context.Context, endpoint string, opts ...Option) (RoundTripper, error) {
	d := net.Dialer{
		Timeout: defaultTimeout,
	}
	cc, err := d.DialContext(ctx, "tcp", endpoint)
	if err != nil {
		return nil, err
	}
	ch := newChannel(cc, opts...)
	rt := &client{
		SendCloser: ch,
		Options:    DefaultOptions(),
		streams:    make(map[uint32]*clientStream),
	}
	for _, o := range opts {
		o(&rt.Options)
	}
	go func() {
		if err := rt.Handle(context.WithoutCancel(ctx), ch); err != nil {
			slog.Debug("transport: receive loop exited", "endpoint", endpoint, "error", err)
		}
	}()
	return rt, nil
}

// Handle runs the receive loop: it reads and dispatches frames until the
// connection breaks or ctx is canceled, then closes the transport. DialContext
// starts it with a context that never ends, so the dialing context has no say
// over the connection's lifetime: Close is the only way down.
func (c *client) Handle(ctx context.Context, recv Recver) error {
	// The receive loop owns the connection; a close error here has nobody left
	// to report to, and the loop's own error is what it returns.
	defer func() { _ = c.Close() }()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := c.interceptor(ctx, recv); err != nil {
				return err
			}
		}
	}
}

// interceptor reads one response frame, resolves the stream by its id and hands
// the header and payload to it. A frame whose stream is already gone is
// discarded so the next frame stays on a boundary.
func (c *client) interceptor(ctx context.Context, recv Recver) error {
	options, err := decodeCallOptions(recv)
	if err != nil {
		return err
	}
	s := c.getStream(options.StreamID)
	if s == nil {
		// the stream is already gone; read the payload frame off the wire to
		// keep the next frame on a boundary, then give the buffer back
		p, err := recv.Recv()
		if err != nil {
			return err
		}
		defer p.Free()
		return nil
	}
	return s.Handle(ctx, recv, WithCallOptions(options))
}

// createStream allocates the next stream id, registers it in the stream table
// and derives a cancellable context for it.
//
// It fails with codes.ResourceExhausted once maxStreams is reached, and with
// codes.Unavailable after Close has zeroed the limit: zero is the closed
// marker. An id is only claimed when the slot is free, so a wrap past MaxUint32
// that lands on a live stream is refused instead of overwriting it.
//
// Call options are ignored: Invoke and RoundTrip build and marshal the header
// themselves. The stream context is cut at readTimeout, or at the caller's
// deadline when that is nearer, so a caller deadline further out never widens
// the stream. Options.timeout does not apply here.
func (c *client) createStream(ctx context.Context, _ ...CallOption) (*clientStream, error) {
	c.Lock()
	defer c.Unlock()
	// maxStreams is never zero while the transport is open — WithMaxStreams
	// replaces zero with the default — so a zero limit can only come from
	// Close, and a call on a closed transport fails instead of allocating a
	// stream.
	if c.maxStreams == 0 {
		return nil, status.Unavailable.Err()
	}
	if len(c.streams) >= int(c.maxStreams) {
		return nil, status.ResourceExhausted.Err()
	}
	streamID := c.streamID + 1
	if _, ok := c.streams[streamID]; ok {
		return nil, status.ResourceExhausted.Err()
	}
	timeout := c.readTimeout
	if dl, ok := ctx.Deadline(); ok {
		timeout = min(cmp.Or(time.Until(dl), timeout), timeout)
	}
	childCtx, cancel := context.WithTimeout(ctx, timeout)
	s := clientStream{
		ctx:          childCtx,
		id:           streamID,
		Sender:       c.SendCloser,
		cancel:       cancel,
		deleteStream: c.deleteStream,
		c:            make(chan *reply, 1),
		codec:        c.codec,
	}
	c.streams[s.id] = &s
	c.streamID = streamID
	return &s, nil
}

// NewStream is not implemented: grpcx carries unary calls only, so every stream
// request is rejected with codes.Unimplemented. The method exists to satisfy
// grpc.ClientConnInterface; desc, method and opts are ignored.
func (c *client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

// deleteStream removes the stream from the table so no further frame is
// routed to it.
func (c *client) deleteStream(id uint32) {
	c.Lock()
	defer c.Unlock()
	delete(c.streams, id)
}

// getStream looks up the stream by id, returning nil when it is already gone.
func (c *client) getStream(sid uint32) *clientStream {
	c.RLock()
	defer c.RUnlock()
	return c.streams[sid]
}

// Invoke performs one unary call: it encodes req, hands the bytes to RoundTrip,
// then decodes the reply into reply. The header, the stream id, the caller
// deadline and the status check all belong to RoundTrip. grpc.CallOption values
// are accepted for interface compatibility and ignored; metadata and the
// deadline travel through ctx.
func (c *client) Invoke(ctx context.Context, method string, req any, reply any, _ ...grpc.CallOption) error {
	b, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}
	response, err := c.RoundTrip(ctx, b, &CallOptions{
		FullMethodName: method,
	})
	if err != nil {
		return err
	}
	defer response.Free()
	if err := c.codec.Unmarshal(response.ReadOnlyData(), reply); err != nil {
		return err
	}
	return nil
}

// RoundTrip sends p as one payload frame on a fresh stream and returns the
// reply payload as a Buffer the caller owns and frees. p goes on the wire as
// given, which lets a relay forward bytes it cannot interpret.
//
// The header is built here: opts first, then the caller's context deadline
// folded into Timeout, then context metadata appended to Metadatas, and the
// stream id last, so this transport's id always wins. A reply whose code is
// not OK is freed here and comes back as a status error with that code and
// description.
func (c *client) RoundTrip(ctx context.Context, p []byte, opts ...CallOption) (Buffer, error) {
	s, err := c.createStream(ctx)
	if err != nil {
		return nil, err
	}
	// The half-close is best effort: a failure to send it is already covered by
	// whatever error the call returns.
	defer func() { _ = s.CloseSend() }()
	in := api.CallOptions{}
	for _, o := range opts {
		o.apply(&in)
	}
	if dl, ok := ctx.Deadline(); ok {
		in.Timeout = time.Until(dl).Milliseconds()
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		for k, v := range medatas {
			in.Metadatas = append(in.Metadatas, k, v)
		}
	}
	// Written last, after every option was applied, so a caller cannot route
	// frames to another stream: the id this transport allocated is the only one
	// the peer will see.
	in.StreamID = s.id
	h, err := encoding.Marshal(&in)
	if err != nil {
		return nil, err
	}
	if err := s.Sender.Send(h, p); err != nil {
		return nil, err
	}
	reply, err := s.recv()
	if err != nil {
		return nil, err
	}
	var out api.CallOptions
	if reply.CallOption != nil {
		reply.apply(&out)
	}
	// A failed call carries no payload the caller wants — the peer answers with
	// api.Empty — so the buffer goes back to the pool here and the caller only
	// gets the status error.
	if code := codes.Code(out.Code); code != codes.OK {
		reply.Free()
		return nil, status.Error(code, out.Desc)
	}
	// The reply buffer is the caller's from here on. Free hands it to the pool
	// unless it is too small to keep: the pool drops a buffer below its 1KB
	// pooling threshold.
	return reply.Buffer, nil
}

// Ping keeps the connection active by opening a stream, sending an empty
// request and waiting for the reply. The balancer relies on it, so an idle
// connection never reaches the read deadline.
//
// Its budget is min(Timeout(), Keepalive()), not the stream's read timeout: a
// health check has to fail before the next one is due. Under the defaults that
// is defaultTimeout, a third of the keepalive period.
func (c *client) Ping(ctx context.Context) error {
	childCtx, cancel := context.WithTimeout(ctx, min(c.Timeout(), c.Keepalive()))
	defer cancel()
	s, err := c.createStream(childCtx)
	if err != nil {
		return err
	}
	// The half-close is best effort: a failure to send it is already covered by
	// whatever error the call returns.
	defer func() { _ = s.CloseSend() }()
	if err := s.Send(&api.Empty{}); err != nil {
		return err
	}
	// The request carries no method name, so the server can only answer
	// codes.Unimplemented. That answer is proof the connection is alive, so it
	// counts as success; every other error is propagated.
	if err := s.RecvMsg(&api.Empty{}); err != nil && status.Code(err) != codes.Unimplemented {
		return err
	}
	return nil
}

// Close tears the transport down once: it cancels every live stream, empties
// the stream table, then closes the connection, so a second call is a no-op.
//
// A call in flight is woken by the stream's cancel and reports codes.Canceled —
// codes.Unavailable when its own deadline fired first. It cannot tell whether
// the peer ran the handler, so it does not report a result.
//
// The limit is then zeroed as the closed marker: WithMaxStreams never stores
// zero, so zero can only mean closed, and createStream refuses every later
// call with codes.Unavailable. The close error is dropped: the connection is
// already being thrown away.
func (c *client) Close() error {
	c.Once.Do(func() {
		defer c.SendCloser.Close()
		c.Lock()
		defer c.Unlock()
		for _, s := range c.streams {
			_ = s.Close()
		}
		clear(c.streams)
		// Zero is the closed marker, not a "no cap" request: createStream compares
		// against it, and WithMaxStreams can never produce it.
		c.maxStreams = 0
	})
	return nil
}

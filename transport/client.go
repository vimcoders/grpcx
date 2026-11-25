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

// WithTimeout sets the default call timeout advertised in the request header.
// Zero falls back to defaultTimeout.
func WithTimeout(d time.Duration) Option {
	return func(t *Options) {
		t.timeout = cmp.Or(d, defaultTimeout)
	}
}

// WithReadTimeout sets how long a channel waits for the next frame before
// giving up on the connection. Both sides use it: ServerOptions.Handle passes it
// to every accepted connection's channel, and the client's own channel reads
// replies under it. It defaults to 2*keepalive; zero leaves the default in
// place, and any other value is stored as given.
func WithReadTimeout(d time.Duration) Option {
	return func(so *Options) {
		so.readTimeout = cmp.Or(d, defaultReadTimeout)
	}
}

// WithWriteTimeout sets how long a write may take to flush before failing the
// call. Both sides use it: ServerOptions.Handle passes it to every accepted
// connection's channel, and the client's own channel writes requests under it.
// It defaults to defaultTimeout; zero leaves the default in place, and any other
// value is stored as given.
func WithWriteTimeout(d time.Duration) Option {
	return func(so *Options) {
		so.writeTimeout = cmp.Or(d, defaultWriteTimeout)
	}
}

// WithCodec sets the codec used to marshal requests and unmarshal responses.
func WithCodec(c encoding.Codec) Option {
	return func(t *Options) {
		t.codec = c
	}
}

// WithBufSize sets the send buffer in blocks of defaultBufSize, clamped to
// defaultMaxBufSize. A value of zero disables batching and makes every Send
// synchronous.
func WithBufSize(n int) Option {
	return func(c *Options) {
		c.bufsize = min(n*defaultBufSize, defaultMaxBufSize)
	}
}

// Options holds the transport configuration shared by the client and the
// server sides.
type Options struct {
	maxStreams    uint32
	timeout       time.Duration
	readTimeout   time.Duration
	writeTimeout  time.Duration
	flushInterval time.Duration
	codec         encoding.Codec
	bufsize       int
}

// Keepalive returns the period at which an idle connection is pinged to keep it
// alive. It is a fixed multiple of defaultTimeout rather than a configurable
// field, so every connection is pinged at the same cadence.
func (o Options) Keepalive() time.Duration {
	return defaultKeepalive
}

// Timeout returns the default call timeout.
func (o Options) Timeout() time.Duration {
	return o.timeout
}

// DefaultOptions returns the transport configuration with every field set to
// its package default.
func DefaultOptions() Options {
	return Options{
		timeout:       defaultTimeout,
		maxStreams:    defaultMaxStreams,
		codec:         encoding.GetCodec(encoding.Name),
		readTimeout:   defaultReadTimeout,
		writeTimeout:  defaultWriteTimeout,
		flushInterval: defaultFlushInterval,
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

// DialContext connects to endpoint over TCP, applies the options to both the
// transport configuration and the connection, then starts the receive loop.
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
	go func() { //nolint:gosec // G118: the receive loop belongs to the connection, so it outlives the caller's context.
		if err := rt.Handle(context.Background(), ch); err != nil {
			slog.Debug("transport: receive loop exited", "endpoint", endpoint, "error", err)
		}
	}()
	return rt, nil
}

// Handle runs the receive loop: it keeps reading frames and dispatching them
// until the connection breaks or ctx is canceled, then closes the transport.
// DialContext starts it with context.Background(), so the ctx that dialed a
// connection has no say over how long that connection lives: the loop's lifetime
// is the connection's. Close is how it ends — it closes the connection, which is
// what the next read reports — and that is the only way down.
func (c *client) Handle(ctx context.Context, recv Recver) error {
	// The receive loop is the owner of the connection; a close error here has
	// nobody left to report to, and the loop's own error is what it returns.
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
// discarded so the next frame stays aligned to a boundary.
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
		putmbuf(p)
		return nil
	}
	return s.Handle(ctx, recv, WithCallOptions(options))
}

// createStream allocates the next stream id, registers it in the stream table
// and derives a cancellable context for it. It fails with
// codes.ResourceExhausted once maxStreams is reached, and with codes.Unavailable
// after Close has zeroed the limit: zero is the closed marker, so a dead
// transport never hands out a stream. An id is only claimed when the slot is
// free, so a wrap past MaxUint32 that lands on a live stream is refused rather
// than overwriting it. Call options are ignored here: Invoke and RoundTrip build
// and marshal the request header themselves, so opts never reach the wire through
// this path.
func (c *client) createStream(ctx context.Context, opts ...CallOption) (*clientStream, error) {
	options := &api.CallOptions{
		Timeout: c.timeout.Milliseconds(),
	}
	for _, opt := range opts {
		opt.apply(options)
	}
	c.Lock()
	defer c.Unlock()
	// maxStreams is never zero while the transport is open — WithMaxStreams
	// replaces zero with the default — so a zero limit can only come from Close,
	// and a call on a closed transport fails instead of allocating a stream.
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
	childCtx, cancel := context.WithTimeout(ctx, time.Millisecond*time.Duration(options.Timeout))
	s := clientStream{
		ctx:          childCtx,
		id:           streamID,
		Sender:       c.SendCloser,
		cancel:       cancel,
		deleteStream: c.deleteStream,
		c:            make(chan *api.Response, 1),
		codec:        c.codec,
	}
	c.streams[s.id] = &s
	c.streamID = streamID
	return &s, nil
}

// NewStream is not implemented: grpcx carries unary calls only, so every
// stream request is rejected with codes.Unimplemented. The method exists solely
// to satisfy grpc.ClientConnInterface; desc, method and opts are all ignored.
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

// Invoke performs one unary call: it opens a stream, sends the request with
// the collected metadata and deadline, then waits for the reply. The
// grpc.CallOption values are accepted for interface compatibility and ignored:
// metadata and the deadline travel through ctx. The header carries a timeout
// only when ctx has a deadline; without one the server falls back to its own
// WithServerTimeout value, while the stream itself still runs under the
// transport's default. A reply whose status code is not OK comes back as a
// status error carrying that code and its description.
func (c *client) Invoke(ctx context.Context, method string, req any, reply any, _ ...grpc.CallOption) error {
	info := CallOptions{
		FullMethodName: method,
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		for k, v := range medatas {
			info.Metadatas = append(info.Metadatas, k, v)
		}
	}
	opts := []CallOption{&info}
	if dl, ok := ctx.Deadline(); ok {
		opt := CallOptions{
			Timeout: time.Until(dl).Milliseconds(),
		}
		opts = append(opts, &opt)
	}
	s, err := c.createStream(ctx, opts...)
	if err != nil {
		return err
	}
	// The half-close is best effort: a failure to send it is already covered by
	// whatever error the call returns.
	defer func() { _ = s.CloseSend() }()
	if err := s.Send(req, opts...); err != nil {
		return err
	}
	return s.RecvMsg(reply)
}

// RoundTrip sends an already framed request — header plus payload — and waits
// for the matching response. The payload is sent as the caller handed it in;
// the header is re-marshalled here, so it carries this transport's stream id and
// default timeout, with the caller's deadline folded in when the context has one.
// A reply whose status code is not OK comes back as a status error carrying that
// code and its description.
func (c *client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	opts := []CallOption{WithCallOptions(cmp.Or(req.Options, &api.CallOptions{}))}
	if dl, ok := ctx.Deadline(); ok {
		opts = append(opts, &CallOptions{
			Timeout: time.Until(dl).Milliseconds(),
		})
	}
	s, err := c.createStream(ctx, opts...)
	if err != nil {
		return nil, err
	}
	// The half-close is best effort: a failure to send it is already covered by
	// whatever error the call returns.
	defer func() { _ = s.CloseSend() }()
	opts = append(opts, &CallOptions{
		StreamID: s.id,
	})
	var options api.CallOptions
	for _, opt := range opts {
		opt.apply(&options)
	}
	h, err := encoding.Marshal(&options)
	if err != nil {
		return nil, err
	}

	if err := s.Sender.Send(h, req.Payload); err != nil {
		return nil, err
	}
	resp, err := s.recv()
	if err != nil {
		return nil, err
	}
	if code := codes.Code(resp.Options.Code); code != codes.OK {
		return nil, status.Error(code, resp.Options.Desc)
	}
	return resp, nil
}

// Ping keeps the connection active by opening a stream, sending an empty
// request and waiting for the reply. It is the health check the balancer relies
// on, so an idle connection never reaches the read deadline and gets torn
// down. The ping has no timeout of its own: the stream carries the transport's
// call timeout (the default is defaultTimeout, 3s, or whatever WithTimeout set),
// so a larger WithTimeout also makes a ping wait longer before it gives up.
func (c *client) Ping(ctx context.Context) error {
	s, err := c.createStream(ctx)
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

// Close tears the transport down once: it closes the underlying connection from
// a defer in the same sync.Once, so a second call is a no-op and nothing is
// closed twice. A call in flight when this runs is deliberately left alone: its
// stream ends on its own deadline and reports codes.Canceled, since no reply
// arrived and nothing here can tell whether the peer ran the handler. The stream table is left as
// it was, but the limit is zeroed as the closed marker: WithMaxStreams never
// stores zero, so zero can only mean "closed" and createStream refuses every
// later call with codes.Unavailable instead of filling a table nothing will
// send from. Closing a connection that is already being thrown away has nothing
// left to report to, so its error is dropped.
func (c *client) Close() error {
	c.Once.Do(func() {
		defer c.SendCloser.Close()
		c.Lock()
		defer c.Unlock()
		// Zero is the closed marker, not a "no cap" request: createStream compares
		// against it, and WithMaxStreams can never produce it.
		c.maxStreams = 0
	})
	return nil
}

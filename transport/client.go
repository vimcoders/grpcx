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

// Keepalive returns the period at which an idle connection is pinged to keep
// it alive.
func (o Options) Keepalive() time.Duration {
	return defaultKeepalive * defaultTimeout
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
	grpc.ClientConnInterface
	Options
	RoundTripper
	sync.RWMutex
	channel     *channel
	streams     map[uint32]*clientStream
	streamID    uint32
	dialContext func(ctx context.Context) (net.Conn, error)
	sync.Once
}

// Dial connects to endpoint over TCP with the background context.
func Dial(endpoint string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

// DialContext connects to endpoint over TCP, applies the options to both the
// transport configuration and the connection, then starts the receive loop.
func DialContext(ctx context.Context, endpoint string, opts ...Option) (RoundTripper, error) {
	dialContext := func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{
			KeepAlive: time.Minute,
		}
		cc, err := d.DialContext(ctx, "tcp", endpoint)
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
	cc, err := dialContext(ctx)
	if err != nil {
		return nil, err
	}
	rt := &client{
		Options:     DefaultOptions(),
		dialContext: dialContext,
		streams:     make(map[uint32]*clientStream),
	}
	for _, o := range opts {
		o(&rt.Options)
	}
	rt.channel = newChannel(cc, opts...)
	go rt.Handle(ctx)
	return rt, nil
}

// Handle runs the receive loop: it keeps reading frames and dispatching them
// until the connection breaks or ctx is canceled, then closes the transport.
func (c *client) Handle(ctx context.Context) error {
	defer c.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := c.interceptor(ctx); err != nil {
				return err
			}
		}
	}
}

// interceptor reads one response frame, resolves the stream by its id and hands
// the header and payload to it. A frame whose stream is already gone is
// discarded so the next frame stays aligned to a boundary.
func (c *client) interceptor(ctx context.Context) error {
	h, err := c.channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(h)
	var opts api.Header
	if err := encoding.Unmarshal(h, &opts); err != nil {
		return err
	}
	s := c.getStream(opts.StreamID)
	if s == nil {
		if _, err := c.channel.discard(); err != nil {
			return err
		}
		return nil
	}
	return s.UnaryInterceptor(ctx, WithUnaryInterceptor(&opts))
}

// createStream allocates the next stream id, registers it in the stream table
// and derives a cancellable context for it. It fails with
// codes.ResourceExhausted once maxStreams is reached.
func (c *client) createStream(ctx context.Context, _ ...CallOption) (*clientStream, error) {
	c.Lock()
	defer c.Unlock()
	if c.maxStreams > 0 && len(c.streams) >= int(c.maxStreams) {
		return nil, status.ResourceExhausted.Err()
	}
	streamID := c.streamID + 1
	if _, ok := c.streams[streamID]; ok {
		return nil, status.ResourceExhausted.Err()
	}
	childCtx, cancel := context.WithCancel(ctx)
	s := clientStream{
		ctx:          childCtx,
		id:           streamID,
		channel:      c.channel,
		cancel:       cancel,
		deleteStream: c.deleteStream,
		c:            make(chan *api.Response, 1),
		codec:        c.codec,
	}
	c.streams[s.id] = &s
	c.streamID = streamID
	return &s, nil
}

// NewStream opens a stream for the given method. Only unary calls are
// supported: desc is ignored and the returned stream carries exactly one
// request and one response.
func (c *client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	info := CallOptions{
		Method:  method,
		Timeout: c.timeout.Milliseconds(),
	}
	return c.createStream(ctx, &info)
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
// the collected metadata and deadline, then waits for the reply.
func (c *client) Invoke(ctx context.Context, method string, req any, reply any, _ ...grpc.CallOption) error {
	info := CallOptions{
		Method:  method,
		Timeout: c.timeout.Milliseconds(),
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
	s, err := c.createStream(ctx)
	if err != nil {
		return err
	}
	defer s.CloseSend()
	if err := s.Send(req, opts...); err != nil {
		return err
	}
	return s.Recv(reply)
}

// RoundTrip sends an already framed request — header plus payload — and waits
// for the matching response. It bypasses the codec, so the caller owns the
// marshalling of both the header and the payload.
func (c *client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	header := cmp.Or(req.Header, &api.Header{})
	if dl, ok := ctx.Deadline(); ok {
		opt := CallOptions{
			Timeout: time.Until(dl).Milliseconds(),
		}
		opt.apply(header)
	}
	s, err := c.createStream(ctx)
	if err != nil {
		return nil, err
	}
	defer s.CloseSend()
	info := CallOptions{
		StreamID: s.id,
		Timeout:  c.timeout.Milliseconds(),
	}
	info.apply(header)
	h, err := encoding.Marshal(header)
	if err != nil {
		return nil, err
	}

	if err := s.channel.Send(h, req.Payload); err != nil {
		return nil, err
	}
	return s.recv()
}

// Ping keeps the connection active by opening a stream, sending an empty
// request and waiting for the reply. It is the health check the balancer relies
// on, so an idle connection never reaches the read deadline and gets torn
// down.
func (c *client) Ping(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	s, err := c.createStream(timeoutCtx)
	if err != nil {
		return err
	}
	defer s.CloseSend()
	if err := s.Send(&api.Empty{}); err != nil {
		return err
	}
	// The request carries no method name, so the server can only answer
	// codes.Unimplemented. That answer is proof the connection is alive, so it
	// counts as success; every other error is propagated.
	if err := s.Recv(&api.Empty{}); err != nil && status.Code(err) != codes.Unimplemented {
		return err
	}
	return nil
}

// Close tears the transport down once: it closes every live stream, empties
// the table and closes the underlying connection.
func (c *client) Close() error {
	c.Once.Do(func() {
		c.Lock()
		defer c.Unlock()
		for _, s := range c.streams {
			s.Close()
		}
		clear(c.streams)
		c.maxStreams = 0
	})
	return c.channel.Close()
}

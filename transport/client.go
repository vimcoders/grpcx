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
	"context"
	"math"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx/encoding"

	"google.golang.org/grpc"
)

const (
	// DefaultMaxStreams is the default maximum number of concurrent streams per connection.
	defaultMaxStreams = math.MaxUint16
	// DefaultTimeout is the default timeout for each request.
	defaultTimeout = 3 * time.Second
)

// RoundTripper is the interface for sending and receiving messages over a transport.
type Option func(*client)

// WithCodec sets the codec for the ttrpc transport.
func WithMaxStreams(n uint32) Option {
	return func(t *client) {
		t.maxStreams = n
	}
}

// WithCodec sets the codec for the ttrpc transport.
func WithTimeout(d time.Duration) Option {
	return func(t *client) {
		t.timeout = d
	}
}

// WithCodec sets the codec for the ttrpc transport.
func WithCodec(c encoding.Codec) Option {
	return func(t *client) {
		t.Codec = c
	}
}

func WithBufSize(n int) Option {
	return func(c *client) {
		c.bufsize = min(n*defaultBufSize, defaultMaxBufSize)
	}
}

// Transport is the implementation of RoundTripper for ttrpc.
type client struct {
	RoundTripper
	sync.RWMutex
	encoding.Codec
	Sender
	streams     map[uint32]*clientStream
	streamID    uint32
	maxStreams  uint32
	ctx         context.Context
	cancel      context.CancelFunc
	dialContext func(ctx context.Context) (net.Conn, error)
	timeout     time.Duration
	bufsize     int
	sync.Once
}

// Dial creates a new ttrpc transport to the given endpoint.
func Dial(endpoint string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

// DialContext creates a new ttrpc transport to the given endpoint with the given context.
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
	childCtx, cancel := context.WithCancel(ctx)
	rt := &client{
		ctx:         childCtx,
		cancel:      cancel,
		dialContext: dialContext,
		timeout:     defaultTimeout,
		maxStreams:  defaultMaxStreams,
		streams:     make(map[uint32]*clientStream),
		Codec:       encoding.GetCodec(encoding.Name),
	}
	for _, o := range opts {
		o(rt)
	}
	channel := newChannelSize(cc, rt.bufsize)
	rt.Sender = channel
	go rt.Handle(ctx, channel)
	return rt, nil
}

// run runs the receive loop for the transport. It receives messages from the channel and dispatches them to the appropriate stream. If the context is canceled, it closes the transport and returns an error.
func (c *client) Handle(ctx context.Context, channel *channel) error {
	defer c.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := c.recv(channel); err != nil {
				return err
			}
		}
	}
}

func (c *client) recv(channel *channel) error {
	streamID, h, err := channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(h)
	_, p, err := channel.Recv()
	if err != nil {
		return err
	}
	s := c.getStream(streamID)
	if s == nil {
		putmbuf(p)
		return nil
	}
	var header api.Header
	if err := encoding.Unmarshal(h, &header); err != nil {
		return err
	}
	select {
	case s.c <- &api.Response{Header: &header, Payload: p}:
		return nil
	default:
		defer putmbuf(p)
		return s.CloseSend()
	}
}

// createStream creates a new stream with the given context. It returns an error if the maximum number of streams has been reached or if the context is canceled.
func (c *client) createStream(ctx context.Context, opts ...CallOption) (*clientStream, error) {
	options := defaultCallOptions
	for _, opt := range opts {
		opt.apply(&options)
	}
	if dl, ok := ctx.Deadline(); ok {
		opt := CallOptions{
			Timeout: time.Until(dl),
		}
		opt.apply(&options)
	}
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
		Sender:       c.Sender,
		CallOptions:  options,
		cancel:       cancel,
		deleteStream: c.deleteStream,
		c:            make(chan *api.Response, 1),
	}
	c.streams[s.id] = &s
	c.streamID = streamID
	return &s, nil
}

// NewStream creates a new stream with the given context. It returns an error if the maximum number of streams has been reached or if the context is canceled.
func (c *client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	info := CallOptions{
		Method:  method,
		Timeout: c.timeout,
		Codec:   c.Codec,
	}
	return c.createStream(ctx, &info)
}

// deleteStream deletes the given stream from the transport. It closes the stream and removes it from the map of streams.
func (c *client) deleteStream(id uint32) {
	c.Lock()
	defer c.Unlock()
	delete(c.streams, id)
}

// getStream returns the stream with the given stream ID. It returns nil if the stream does not exist.
func (c *client) getStream(sid uint32) *clientStream {
	c.RLock()
	defer c.RUnlock()
	return c.streams[sid]
}

// Invoke invokes the given method with the given request and response. It marshals the request, sends it to the server, and unmarshals the response. If the context is canceled, it returns an error.
func (c *client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	info := CallOptions{
		Method:  method,
		Timeout: c.timeout,
		Codec:   c.Codec,
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		info.Metadatas = medatas
	}
	s, err := c.createStream(ctx, &info)
	if err != nil {
		return err
	}
	defer s.CloseSend()
	if err := s.SendMsg(req); err != nil {
		return err
	}
	if err := s.RecvMsg(reply); err != nil {
		return err
	}
	return nil
}

// RoundTrip sends the given request to the server and returns the response. It creates a new stream, sends the request, and waits for the response. If the context is canceled, it returns an error.
func (c *client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	s, err := c.createStream(ctx, &CallOptions{})
	if err != nil {
		return nil, err
	}
	defer s.CloseSend()
	if err := s.send(req); err != nil {
		return nil, err
	}
	return s.recv()
}

func (c *client) Ping(ctx context.Context) error {
	timeoutCtx, cancel := context.WithTimeout(ctx, c.timeout)
	defer cancel()
	if _, err := c.RoundTrip(timeoutCtx, &api.Request{}); err != nil {
		return err
	}
	return nil
}

// Close closes the ttrpc connection and underlying connection
func (c *client) Close() error {
	if c.cancel != nil {
		c.cancel()
	}
	return nil
}

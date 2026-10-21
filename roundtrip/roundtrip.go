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
package roundtrip

import (
	"context"
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
	defaultMaxStreams = 64
	// DefaultTimeout is the default timeout for each request.
	defaultTimeout = 3 * time.Second
)

// RoundTripper is the interface for sending and receiving messages over a transport.
type Option func(*roundtrip)

// WithCodec sets the codec for the ttrpc transport.
func WithMaxStreams(n uint32) Option {
	return func(t *roundtrip) {
		t.maxStreams = n
	}
}

// WithCodec sets the codec for the ttrpc transport.
func WithTimeout(d time.Duration) Option {
	return func(t *roundtrip) {
		t.timeout = d
	}
}

// WithCodec sets the codec for the ttrpc transport.
func WithCodec(c encoding.Codec) Option {
	return func(t *roundtrip) {
		t.Codec = c
	}
}

// Transport is the implementation of RoundTripper for ttrpc.
type roundtrip struct {
	RoundTripper
	sync.RWMutex
	encoding.Codec
	channel     *channel
	streams     map[uint32]*stream
	streamID    uint32
	maxStreams  uint32
	ctx         context.Context
	closed      func()
	dialContext func(ctx context.Context) (net.Conn, error)
	timeout     time.Duration
}

// Dial creates a new ttrpc transport to the given target.
func Dial(target string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), target, opts...)
}

// DialContext creates a new ttrpc transport to the given target with the given context.
func DialContext(ctx context.Context, target string, opts ...Option) (RoundTripper, error) {
	dialContext := func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{
			KeepAlive: time.Minute,
		}
		cc, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, err
		}
		return cc, nil
	}
	cc, err := dialContext(ctx)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	rt := &roundtrip{
		channel:     newChannel(cc),
		maxStreams:  defaultMaxStreams,
		ctx:         ctx,
		closed:      cancel,
		streams:     make(map[uint32]*stream),
		Codec:       encoding.GetCodec(encoding.Name),
		dialContext: dialContext,
		timeout:     defaultTimeout,
	}
	for _, o := range opts {
		o(rt)
	}
	go func() {
		_ = rt.run(ctx)
	}()
	return rt, nil
}

// run runs the receive loop for the transport. It receives messages from the channel and dispatches them to the appropriate stream. If the context is canceled, it closes the transport and returns an error.
func (t *roundtrip) run(ctx context.Context) error {
	defer t.Close()
	defer t.channel.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := t.recv(ctx); err != nil {
				return err
			}
		}
	}
}

func (t *roundtrip) recv(ctx context.Context) error {
	streamID, b, err := t.channel.Recv()
	if err != nil {
		return err
	}
	defer t.channel.putmbuf(b)
	s := t.getStream(streamID)
	if s == nil {
		return nil
	}
	if err := s.process(ctx, b); err != nil {
		return err
	}
	return nil
}

// createStream creates a new stream with the given context. It returns an error if the maximum number of streams has been reached or if the context is canceled.
func (t *roundtrip) createStream(ctx context.Context, opts ...CallOption) (*stream, error) {
	t.Lock()
	defer t.Unlock()
	if t.maxStreams > 0 && len(t.streams) >= int(t.maxStreams) {
		return nil, status.ResourceExhausted.Err()
	}
	for i := uint32(1); i < t.maxStreams; i++ {
		select {
		case <-ctx.Done():
			return nil, status.Canceled.Err()
		default:
			streamID := t.streamID + i
			if _, ok := t.streams[streamID]; ok {
				continue
			}
			s := newStream(ctx, streamID, t.channel, opts...)
			t.streams[s.id] = s
			t.streamID = streamID
			return s, nil
		}
	}
	return nil, status.ResourceExhausted.Err()
}

// NewStream creates a new stream with the given context. It returns an error if the maximum number of streams has been reached or if the context is canceled.
func (t *roundtrip) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

// deleteStream deletes the given stream from the transport. It closes the stream and removes it from the map of streams.
func (t *roundtrip) deleteStream(id uint32) {
	t.Lock()
	defer t.Unlock()
	delete(t.streams, id)
}

// getStream returns the stream with the given stream ID. It returns nil if the stream does not exist.
func (t *roundtrip) getStream(sid uint32) *stream {
	t.RLock()
	defer t.RUnlock()
	s := t.streams[sid]
	return s
}

// cleanupStreams closes all streams and removes them from the transport. It is called when the transport is closed.
func (t *roundtrip) cleanupStreams() {
	t.Lock()
	defer t.Unlock()
	for sid := range t.streams {
		delete(t.streams, sid)
	}
}

// Invoke invokes the given method with the given request and response. It marshals the request, sends it to the server, and unmarshals the response. If the context is canceled, it returns an error.
func (t *roundtrip) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	info := CallOptions{
		Method:       method,
		Timeout:      t.timeout,
		Codec:        t.Codec,
		deleteStream: t.deleteStream,
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		info.Metadatas = medatas
	}
	s, err := t.createStream(ctx, &info)
	if err != nil {
		return err
	}
	defer s.Close()
	if err := s.SendMsg(req); err != nil {
		return err
	}
	if err := s.RecvMsg(reply); err != nil {
		return err
	}
	return nil
}

// RoundTrip sends the given request to the server and returns the response. It creates a new stream, sends the request, and waits for the response. If the context is canceled, it returns an error.
func (t *roundtrip) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	info := CallOptions{
		Timeout:      t.timeout,
		Codec:        t.Codec,
		deleteStream: t.deleteStream,
	}
	s, err := t.createStream(ctx, &info)
	if err != nil {
		return nil, err
	}
	defer s.Close()
	if err := s.send(ctx, req); err != nil {
		return nil, err
	}
	return s.recv(ctx)
}

// Close closes the ttrpc connection and underlying connection
func (t *roundtrip) Close() error {
	if t.closed != nil {
		t.closed()
	}
	t.cleanupStreams()
	return nil
}

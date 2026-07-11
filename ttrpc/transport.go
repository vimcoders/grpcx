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
package ttrpc

import (
	"context"
	"grpcx/generated/api"
	"grpcx/metadata"
	"grpcx/status"
	"math"
	"net"
	"sync"
	"time"

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type Option func(*Transport)

func WithMaxStreams(n int) Option {
	return func(t *Transport) {
		t.maxStreams = n
	}
}

func WithTimeout(d time.Duration) Option {
	return func(t *Transport) {
		t.timeout = d
	}
}

type Transport struct {
	RoundTripper
	sync.RWMutex
	encoding.Codec
	c           net.Conn
	channel     *channel
	streams     map[uint32]*stream
	streamID    uint32
	maxStreams  int
	ctx         context.Context
	closed      func()
	dialContext func(ctx context.Context) (net.Conn, error)
	timeout     time.Duration
}

func Dial(target string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), target, opts...)
}

const (
	// DefaultMaxStreams is the default maximum number of concurrent streams per connection.
	defaultMaxStreams = 64
	// DefaultTimeout is the default timeout for each request.
	defaultTimeout = 3 * time.Second
)

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
	rt := &Transport{
		channel:     newChannel(cc),
		c:           cc,
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
		rt.run(ctx)
	}()
	return rt, nil
}

func (t *Transport) run(ctx context.Context) error {
	return t.receiveLoop(ctx)
}

func (t *Transport) receiveLoop(ctx context.Context) error {
	defer t.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			streamID, payload, err := t.channel.Recv()
			if err != nil {
				return err
			}
			s := t.getStream(streamID)
			if s == nil {
				t.channel.putmbuf(payload)
				continue
			}
			var response api.Response
			if err := t.Unmarshal(payload, &response); err != nil {
				s.close()
				t.channel.putmbuf(payload)
				continue
			}
			t.channel.putmbuf(payload)
			if err := s.receive(ctx, &response); err != nil {
				continue
			}
		}
	}
}

func (t *Transport) createStream(ctx context.Context) (*stream, error) {
	t.Lock()
	defer t.Unlock()

	select {
	case <-ctx.Done():
		return nil, status.Canceled.Err()
	default:
		if t.maxStreams > 0 && len(t.streams) >= t.maxStreams {
			return nil, status.ResourceExhausted.Err()
		}
		for i := uint32(1); i < math.MaxInt8; i++ {
			streamID := t.streamID + i
			if _, ok := t.streams[streamID]; ok {
				continue
			}
			s := newStream(streamID, t.channel)
			t.streams[s.id] = s
			t.streamID = streamID
			return s, nil
		}
	}
	return nil, status.ResourceExhausted.Err()
}

func (t *Transport) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return t.createStream(ctx)
}

func (t *Transport) deleteStream(s *stream) {
	t.Lock()
	defer t.Unlock()
	delete(t.streams, s.id)
	s.close()
}

func (t *Transport) getStream(sid uint32) *stream {
	t.RLock()
	defer t.RUnlock()
	s := t.streams[sid]
	return s
}

func (t *Transport) cleanupStreams() {
	t.Lock()
	defer t.Unlock()
	for sid, s := range t.streams {
		delete(t.streams, sid)
		s.close()
	}
}

func (t *Transport) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := t.Marshal(req)
	if err != nil {
		return err
	}
	request := &api.Request{
		Method:  method,
		Payload: payload,
		Timeout: t.timeout.Milliseconds(),
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		for k, v := range medatas {
			request.Metadatas = append(request.Metadatas, k, v)
		}
	}
	response, err := t.RoundTrip(ctx, request)
	if err != nil {
		return err
	}
	code := codes.Code(response.Code)
	if code != codes.OK {
		return status.Error(code, response.Message)
	}
	if err = t.Unmarshal(response.Payload, reply); err != nil {
		return err
	}
	return nil
}

func (t *Transport) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	timeoutCtx, cancel := context.WithTimeout(ctx, t.timeout)
	defer cancel()
	b, err := t.Marshal(req)
	if err != nil {
		return nil, err
	}
	s, err := t.createStream(timeoutCtx)
	if err != nil {
		return nil, err
	}
	defer t.deleteStream(s)
	if err := s.send(timeoutCtx, b); err != nil {
		return nil, err
	}
	select {
	case <-timeoutCtx.Done():
		return nil, status.Canceled.Err()
	case <-t.ctx.Done():
		return nil, status.Canceled.Err()
	case msg, ok := <-s.recv:
		if !ok {
			return nil, status.Unavailable.Err()
		}
		return msg, nil
	}
}

// Close closes the ttrpc connection and underlying connection
func (t *Transport) Close() error {
	if t.closed != nil {
		t.closed()
	}
	t.cleanupStreams()
	return nil
}

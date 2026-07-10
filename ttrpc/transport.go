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

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type Option func(*Transport)

func MaxStreams(n int) Option {
	return func(t *Transport) {
		t.maxStreams = n
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
	retries     int32
	dialContext func(ctx context.Context) (net.Conn, error)
}

func Dial(target string, opts ...Option) (RoundTripper, error) {
	return DialContext(context.Background(), target, opts...)
}

func DialContext(ctx context.Context, target string, opts ...Option) (RoundTripper, error) {
	dialContext := func(ctx context.Context) (net.Conn, error) {
		d := net.Dialer{}
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
		streamID:    1,
		maxStreams:  64, // 默认最大64个并发流
		retries:     3,
		ctx:         ctx,
		closed:      cancel,
		streams:     make(map[uint32]*stream),
		Codec:       encoding.GetCodec(encoding.Name),
		dialContext: dialContext,
	}
	for _, o := range opts {
		o(rt)
	}
	go func() {
		rt.run(ctx)
	}()
	return rt, nil
}

func (c *Transport) run(ctx context.Context) {
	if err := c.receiveLoop(ctx); err != nil {
		if status.Code(err) == status.Canceled.Code() {
			return
		}
	}
	for range c.retries {
		cc, err := c.dialContext(ctx)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.channel, c.c, c.ctx, c.closed = newChannel(cc), cc, ctx, cancel
		c.run(ctx)
	}
}

func (c *Transport) receiveLoop(ctx context.Context) error {
	defer c.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			streamID, payload, err := c.channel.Recv()
			if err != nil {
				return err
			}
			s := c.getStream(streamID)
			if s == nil {
				c.channel.putmbuf(payload)
				continue
			}
			var response api.Response
			if err := c.Unmarshal(payload, &response); err != nil {
				s.close()
				c.channel.putmbuf(payload)
				continue
			}
			c.channel.putmbuf(payload)
			if err := s.receive(ctx, &response); err != nil {
				continue
			}
		}
	}
}

func (c *Transport) createStream(ctx context.Context) (*stream, error) {
	c.Lock()
	defer c.Unlock()

	select {
	case <-ctx.Done():
		return nil, status.Canceled.Err()
	default:
		if c.maxStreams > 0 && len(c.streams) >= c.maxStreams {
			return nil, status.ResourceExhausted.Err()
		}
		for i := uint32(1); i < math.MaxInt8; i++ {
			streamID := c.streamID + i
			if _, ok := c.streams[streamID]; ok {
				continue
			}
			s := newStream(streamID, c.channel)
			c.streams[s.id] = s
			c.streamID = streamID
			return s, nil
		}
	}
	return nil, status.ResourceExhausted.Err()
}

func (c *Transport) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return c.createStream(ctx)
}

func (c *Transport) deleteStream(s *stream) {
	c.Lock()
	defer c.Unlock()
	delete(c.streams, s.id)
	s.close()
}

func (c *Transport) getStream(sid uint32) *stream {
	c.RLock()
	defer c.RUnlock()
	s := c.streams[sid]
	return s
}

func (c *Transport) cleanupStreams() {
	c.Lock()
	defer c.Unlock()
	for sid, s := range c.streams {
		delete(c.streams, sid)
		s.close()
	}
}

func (c *Transport) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.Marshal(req)
	if err != nil {
		return err
	}
	request := &api.Request{
		Method:  method,
		Payload: payload,
	}
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		for k, v := range medatas {
			request.Metadatas = append(request.Metadatas, k, v)
		}
	}
	response, err := c.RoundTrip(ctx, request)
	if err != nil {
		return err
	}
	code := codes.Code(response.Code)
	if code != codes.OK {
		return status.Error(code, response.Message)
	}
	if err = c.Unmarshal(response.Payload, reply); err != nil {
		return err
	}
	return nil
}

func (c *Transport) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	b, err := c.Marshal(req)
	if err != nil {
		return nil, err
	}
	s, err := c.createStream(ctx)
	if err != nil {
		return nil, err
	}
	if err := s.send(ctx, b); err != nil {
		return nil, err
	}
	defer c.deleteStream(s)
	select {
	case <-ctx.Done():
		return nil, status.Canceled.Err()
	case <-c.ctx.Done():
		return nil, status.Canceled.Err()
	case msg, ok := <-s.recv:
		if !ok {
			return nil, status.Unavailable.Err()
		}
		return msg, nil
	}
}

// Close closes the ttrpc connection and underlying connection
func (c *Transport) Close() error {
	if c.closed != nil {
		c.closed()
	}
	c.cleanupStreams()
	return nil
}

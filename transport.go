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
	"log/slog"
	"net"
	"sync"
	"ttrpc/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

// RoundTripper is an interface representing the ability to execute a
// single HTTP transaction, obtaining the [Response] for a given [Request].
//
// A RoundTripper must be safe for concurrent use by multiple
// goroutines.
type RoundTripper interface {
	// RoundTrip executes a single HTTP transaction, returning
	// a Response for the provided Request.
	//
	// RoundTrip should not attempt to interpret the response. In
	// particular, RoundTrip must return err == nil if it obtained
	// a response, regardless of the response's HTTP status code.
	// A non-nil err should be reserved for failure to obtain a
	// response. Similarly, RoundTrip should not attempt to
	// handle higher-level protocol details such as redirects,
	// authentication, or cookies.
	//
	// RoundTrip should not modify the request, except for
	// consuming and closing the Request's Body. RoundTrip may
	// read fields of the request in a separate goroutine. Callers
	// should not mutate or reuse the request until the Response's
	// Body has been closed.
	//
	// RoundTrip must always close the body, including on errors,
	// but depending on the implementation may do so in a separate
	// goroutine even after RoundTrip returns. This means that
	// callers wanting to reuse the body for subsequent requests
	// must arrange to wait for the Close call before doing so.
	//
	// The Request's URL and Header fields must be initialized.
	RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error)
}

type Transport struct {
	sync.RWMutex
	codec    codec
	conn     net.Conn
	channel  *channel
	streams  map[uint32]*stream
	streamID uint32
	ctx      context.Context
	closed   func()
	retries  int32

	interceptor UnaryClientInterceptor
}

type Option func(c *Transport)

func DialContext(ctx context.Context, target string, opts ...Option) (grpc.ClientConnInterface, error) {
	d := net.Dialer{}
	cc, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	channel := newChannel(cc)
	c := &Transport{
		codec:    codec{},
		channel:  channel,
		streams:  make(map[uint32]*stream),
		streamID: 1,
		closed:   cancel,
		ctx:      ctx,
		retries:  3,
	}

	c.interceptor = func(ctx context.Context, r *api.Request, rt RoundTripper) (*api.Response, error) {
		return rt.RoundTrip(ctx, r)
	}

	for _, o := range opts {
		o(c)
	}

	go c.run(ctx)
	return c, nil
}

func Dial(target string, opts ...Option) (grpc.ClientConnInterface, error) {
	return DialContext(context.Background(), target, opts...)
}

func (c *Transport) run(ctx context.Context) {
	if err := c.receiveLoop(ctx); err != nil {
		slog.Error("run", "receiveLoop", err)
	}
	c.cleanupStreams()
	if err := c.Close(); err != nil {
		slog.Error("run", "close", err)
	}
	for range c.retries {
		d := net.Dialer{}
		network, address := c.conn.RemoteAddr().Network(), c.conn.RemoteAddr().String()
		cc, err := d.DialContext(ctx, network, address)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.conn = cc
		c.channel = newChannel(cc)
		c.ctx = ctx
		c.closed = cancel
		c.run(ctx)
	}
}

func (c *Transport) receiveLoop(ctx context.Context) error {
	for {
		select {
		case <-ctx.Done():
			return Canceled.Err()
		default:
			streamID, payload, err := c.channel.recv()
			if err != nil {
				return err
			}
			var response api.Response
			if err := c.codec.Unmarshal(payload, &response); err != nil {
				return err
			}
			c.channel.putmbuf(payload)
			s := c.getStream(streamID)
			if s == nil {
				continue
			}
			if err := s.receive(ctx, &response); err != nil {
				continue
			}
		}
	}
}

// createStream creates a new stream and registers it with the client
// Introduce stream types for multiple or single response
func (c *Transport) createStream(ctx context.Context, b []byte) (*stream, error) {
	// sendLock must be held across both allocation of the stream ID and sending it across the wire.
	// This ensures that new stream IDs sent on the wire are always increasing, which is a
	// requirement of the TTRPC protocol.
	// This use of sendLock could be split into another mutex that covers stream creation + first send,
	// and just use sendLock to guard writing to the wire, but for now it seems simpler to have fewer mutexes.
	c.Lock()
	defer c.Unlock()

	// Check if closed since lock acquired to prevent adding
	// anything after cleanup completes
	select {
	case <-ctx.Done():
		return nil, Canceled.Err()
	default:
		s := newStream(c.streamID, c.channel)
		c.streams[s.id] = s
		c.streamID = c.streamID + 2
		if err := s.send(ctx, b); err != nil {
			return nil, err
		}
		return s, nil
	}
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

func (c *Transport) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	b, err := c.codec.Marshal(req)
	if err != nil {
		return nil, err
	}
	s, err := c.createStream(ctx, b)
	if err != nil {
		return nil, err
	}
	defer c.deleteStream(s)
	select {
	case <-ctx.Done():
		return nil, Canceled.Err()
	case <-c.ctx.Done():
		return nil, Canceled.Err()
	case msg, ok := <-s.recv:
		if !ok {
			return nil, Unavailable.Err()
		}
		return msg, nil
	}
}

func (c *Transport) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}
	for range c.retries {
		response, err := c.interceptor(ctx, &api.Request{
			Metas: &api.Metas{
				Path:      method,
				Authority: GetAuthority(ctx),
			},
			Payload: payload,
		}, c)
		if err != nil {
			return err
		}
		switch codes.Code(response.Code) {
		case codes.OK:
			if err = c.codec.Unmarshal(response.Payload, reply); err != nil {
				return err
			}
			return nil
		case codes.Unavailable:
			continue
		default:
			return status.New(codes.Code(response.Code), response.Message).Err()
		}
	}
	return status.New(codes.Unavailable, codes.Unavailable.String()).Err()
}

// NewStream creates a new stream with the given stream descriptor to the
// specified service and method. If not a streaming client, the request object
// may be provided.
func (c *Transport) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, nil
}

// Close closes the ttrpc connection and underlying connection
func (c *Transport) Close() error {
	if c.closed != nil {
		c.closed()
	}
	return c.conn.Close()
}

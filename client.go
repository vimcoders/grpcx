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
	"google.golang.org/protobuf/proto"
)

type Client struct {
	sync.RWMutex
	codec    codec
	conn     net.Conn
	channel  *channel
	streams  map[uint32]*stream
	streamID uint32
	ctx      context.Context
	closed   func()
	retries  int32
	target   string
}

type ClientOpts func(c *Client)

func DialContext(ctx context.Context, target string, opts ...ClientOpts) (grpc.ClientConnInterface, error) {
	d := net.Dialer{}
	cc, err := d.DialContext(ctx, "tcp", target)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	channel := newChannel(cc)
	c := &Client{
		codec:    codec{},
		channel:  channel,
		streams:  make(map[uint32]*stream),
		streamID: 1,
		closed:   cancel,
		ctx:      ctx,
		retries:  3,
		target:   target,
	}

	for _, o := range opts {
		o(c)
	}

	go c.run(ctx)
	return c, nil
}

func Dial(target string, opts ...ClientOpts) (grpc.ClientConnInterface, error) {
	return DialContext(context.Background(), target, opts...)
}

func (c *Client) run(ctx context.Context) {
	if err := c.receiveLoop(ctx); err != nil {
		slog.Error("run", "receiveLoop", err)
	}
	c.cleanupStreams()
	if err := c.Close(); err != nil {
		slog.Error("run", "close", err)
	}
	for range c.retries {
		d := net.Dialer{}
		cc, err := d.DialContext(ctx, "tcp", c.target)
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

func (c *Client) receiveLoop(ctx context.Context) error {
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
			if err := proto.Unmarshal(payload, &response); err != nil {
				return err
			}
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
func (c *Client) createStream(ctx context.Context) (*stream, error) {
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
		return s, nil
	}
}

func (c *Client) deleteStream(s *stream) {
	c.Lock()
	defer c.Unlock()
	delete(c.streams, s.id)
	s.close()
}

func (c *Client) getStream(sid uint32) *stream {
	c.RLock()
	defer c.RUnlock()
	s := c.streams[sid]
	return s
}

func (c *Client) cleanupStreams() {
	c.Lock()
	defer c.Unlock()
	for sid, s := range c.streams {
		delete(c.streams, sid)
		s.close()
	}
}

func (c *Client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	s, err := c.createStream(ctx)
	if err != nil {
		return nil, err
	}
	defer c.deleteStream(s)
	b, err := c.codec.Marshal(req)
	if err != nil {
		return nil, err
	}
	if err := s.send(ctx, b); err != nil {
		return nil, err
	}
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

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}
	for range c.retries {
		response, err := c.RoundTrip(ctx, &api.Request{
			Metas: &api.Metas{
				Path:      method,
				Authority: GetAuthority(ctx),
			},
			Payload: payload,
		})
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
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, nil
}

// Close closes the ttrpc connection and underlying connection
func (c *Client) Close() error {
	if c.closed != nil {
		c.closed()
	}
	return c.conn.Close()
}

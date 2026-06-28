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
	codec        codec
	conn         net.Conn
	channel      *channel
	streams      map[uint32]*stream
	nextStreamID uint32
	ctx          context.Context
	closed       func()
	closeOnce    sync.Once
	retries      int32
}

type ClientOpts func(c *Client)

// NewClient creates a new ttrpc client using the given connection
func NewClient(conn net.Conn, opts ...ClientOpts) grpc.ClientConnInterface {
	ctx, cancel := context.WithCancel(context.Background())
	channel := newChannel(conn)
	c := &Client{
		codec:        codec{},
		channel:      channel,
		streams:      make(map[uint32]*stream),
		nextStreamID: 1,
		closed:       cancel,
		ctx:          ctx,
		retries:      3,
	}

	for _, o := range opts {
		o(c)
	}

	go c.run()
	return c
}

func (c *Client) run() {
	err := c.receiveLoop()
	if err != nil {
		c.Close()
		c.cleanupStreams(err)
		slog.Error("receiveLoop", "err", err)
	}
}

func (c *Client) receiveLoop() error {
	for {
		select {
		case <-c.ctx.Done():
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
			if err := s.receive(c.ctx, &response); err != nil {
				s.closeWithError(err)
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
	case <-c.ctx.Done():
		return nil, Canceled.Err()
	case <-ctx.Done():
		return nil, Canceled.Err()
	default:
		s := newStream(c.nextStreamID, c.channel)
		c.streams[s.id] = s
		c.nextStreamID = c.nextStreamID + 2
		return s, nil
	}
}

func (c *Client) deleteStream(s *stream) {
	c.Lock()
	defer c.Unlock()
	delete(c.streams, s.id)
	s.closeWithError(nil)
}

func (c *Client) getStream(sid uint32) *stream {
	c.RLock()
	defer c.RUnlock()
	s := c.streams[sid]
	return s
}

func (c *Client) cleanupStreams(err error) {
	c.Lock()
	defer c.Unlock()
	for sid, s := range c.streams {
		s.closeWithError(err)
		delete(c.streams, sid)
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
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, status.New(codes.Canceled, codes.Canceled.String()).Err()
	case <-s.recvClose:
		return nil, status.New(codes.Canceled, codes.Canceled.String()).Err()
	case msg := <-s.recv:
		return msg, nil
	}
}

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}
	for range c.retries {
		response, err := c.RoundTrip(ctx, &api.Request{Metas: &api.Metas{Path: method}, Payload: payload})
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
	c.closeOnce.Do(func() {
		c.closed()

		c.conn.Close()
	})
	return nil
}

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
	"errors"
	"io"
	"net"
	"strings"
	"sync"
	"syscall"
	"ttrpc/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
)

type Client struct {
	codec   codec
	conn    net.Conn
	channel *channel

	streamLock   sync.RWMutex
	streams      map[uint32]*stream
	nextStreamID uint32
	sendLock     sync.Mutex

	ctx    context.Context
	closed func()

	closeOnce sync.Once
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
	}

	for _, o := range opts {
		o(c)
	}

	go c.run()
	return c
}

func (c *Client) run() {
	err := c.receiveLoop()
	c.Close()
	c.cleanupStreams(err)
}

func (c *Client) receiveLoop() error {
	for {
		select {
		case <-c.ctx.Done():
			return ErrClosed
		default:
			h, payload, err := c.channel.recv()
			if err != nil {
				_, ok := status.FromError(err)
				if !ok {
					// treat all errors that are not an rpc status as terminal.
					// all others poison the connection.
					return filterCloseErr(err)
				}
			}
			s := c.getStream(h.StreamID)
			if s == nil {
				continue
			}

			if err != nil {
				s.closeWithError(err)
			} else {
				if err := s.receive(c.ctx, &api.Response{Payload: payload}); err != nil {
				}
			}
		}
	}
}

// createStream creates a new stream and registers it with the client
// Introduce stream types for multiple or single response
func (c *Client) createStream(ctx context.Context, b []byte) (*stream, error) {
	// sendLock must be held across both allocation of the stream ID and sending it across the wire.
	// This ensures that new stream IDs sent on the wire are always increasing, which is a
	// requirement of the TTRPC protocol.
	// This use of sendLock could be split into another mutex that covers stream creation + first send,
	// and just use sendLock to guard writing to the wire, but for now it seems simpler to have fewer mutexes.
	c.sendLock.Lock()
	defer c.sendLock.Unlock()

	// Check if closed since lock acquired to prevent adding
	// anything after cleanup completes
	select {
	case <-c.ctx.Done():
		return nil, ErrClosed
	default:
	}

	var s *stream
	if err := func() error {
		// In the future this could be replaced with a sync.Map instead of streamLock+map.
		c.streamLock.Lock()
		defer c.streamLock.Unlock()

		// Check if closed since lock acquired to prevent adding
		// anything after cleanup completes
		select {
		case <-c.ctx.Done():
			return ErrClosed
		default:
		}

		s = newStream(c.nextStreamID, c.channel)
		c.streams[s.id] = s
		c.nextStreamID = c.nextStreamID + 2

		return nil
	}(); err != nil {
		return nil, err
	}

	if err := s.send(ctx, b); err != nil {
		return s, filterCloseErr(err)
	}

	return s, nil
}

func (c *Client) deleteStream(s *stream) {
	c.streamLock.Lock()
	delete(c.streams, s.id)
	c.streamLock.Unlock()
	s.closeWithError(nil)
}

func (c *Client) getStream(sid uint32) *stream {
	c.streamLock.RLock()
	s := c.streams[sid]
	c.streamLock.RUnlock()
	return s
}

func (c *Client) cleanupStreams(err error) {
	c.streamLock.Lock()
	defer c.streamLock.Unlock()

	for sid, s := range c.streams {
		s.closeWithError(err)
		delete(c.streams, sid)
	}
}

// filterCloseErr rewrites EOF and EPIPE errors to ErrClosed. Use when
// returning from call or handling errors from main read loop.
//
// This purposely ignores errors with a wrapped cause.
func filterCloseErr(err error) error {
	switch {
	case err == nil:
		return nil
	case err == io.EOF:
		return ErrClosed
	case errors.Is(err, io.ErrClosedPipe):
		return ErrClosed
	case errors.Is(err, io.EOF):
		return ErrClosed
	case strings.Contains(err.Error(), "use of closed network connection"):
		return ErrClosed
	default:
		// if we have an epipe on a write or econnreset on a read , we cast to errclosed
		var oerr *net.OpError
		if errors.As(err, &oerr) {
			if (oerr.Op == "write" && errors.Is(err, syscall.EPIPE)) ||
				(oerr.Op == "read" && errors.Is(err, syscall.ECONNRESET)) {
				return ErrClosed
			}
		}
	}

	return err
}

func (c *Client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	b, err := proto.Marshal(req)
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
		return nil, ctx.Err()
	case <-c.ctx.Done():
		return nil, ErrClosed
	case <-s.recvClose:
		return nil, status.Errorf(codes.Canceled, "stream closed")
	case msg := <-s.recv:
		return msg, nil
	}
}

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.codec.Marshal(req)
	if err != nil {
		return err
	}
	response, err := c.RoundTrip(ctx, &api.Request{Metas: &api.Metas{Path: method}, Payload: payload})
	if err != nil {
		return err
	}

	if err = c.codec.Unmarshal(response.Payload, reply); err != nil {
		return err
	}
	return nil
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

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
	"grpcx/generated/api"
	"grpcx/status"
	"log/slog"
	"net"
	"sync"

	"grpcx/encoding"
)

type Option func(*Transport)

func MaxStreams(n uint32) Option {
	return func(t *Transport) {
		t.maxStreams = n
	}
}

type Transport struct {
	sync.RWMutex
	encoding.Codec
	c          net.Conn
	channel    *Channel
	streams    map[uint32]*stream
	streamID   uint32
	maxStreams uint32
	ctx        context.Context
	closed     func()
	retries    int32
}

func NewRoundTripper(c net.Conn, opts ...Option) RoundTripper {
	ctx, cancel := context.WithCancel(context.Background())
	rt := &Transport{
		channel:    newChannel(c),
		c:          c,
		streamID:   1,
		maxStreams: 64, // 默认最大64个并发流
		retries:    3,
		ctx:        ctx,
		closed:     cancel,
		streams:    make(map[uint32]*stream),
		Codec:      encoding.GetCodec(encoding.Name),
	}
	for _, o := range opts {
		o(rt)
	}
	go rt.run(ctx)
	return rt
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
		network, address := c.c.RemoteAddr().Network(), c.c.RemoteAddr().String()
		cc, err := d.DialContext(ctx, network, address)
		if err != nil {
			continue
		}
		ctx, cancel := context.WithCancel(context.Background())
		c.c = cc
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
			return status.Canceled.Err()
		default:
			streamID, payload, err := c.channel.Recv()
			if err != nil {
				return err
			}
			var response api.Response
			if err := c.Unmarshal(payload, &response); err != nil {
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
		return nil, status.Canceled.Err()
	default:
		if c.maxStreams > 0 && uint32(len(c.streams)) >= c.maxStreams {
			return nil, status.ResourceExhausted.Err()
		}
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
	b, err := c.Marshal(req)
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
	return c.c.Close()
}

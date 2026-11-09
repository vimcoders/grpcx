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

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type clientStream struct {
	channel *channel
	grpc.ClientStream
	ctx          context.Context
	id           uint32
	c            chan *api.Response
	cancel       context.CancelFunc
	deleteStream func(id uint32)
	codec        encoding.Codec
	opts         []CallOption
	sync.Once
}

func (s *clientStream) CloseSend() error {
	defer s.Close()
	var header api.Header
	opt := CallOptions{Closed: true}
	opt.apply(&header)
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	s.Once.Do(func() {
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
		_ = s.channel.Send(s.id, h)
	})
	return nil
}

// close closes the stream and releases any resources associated with it.
func (s *clientStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func (s *clientStream) SendMsg(m any) error {
	p, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	return s.send(p)
}

func (s *clientStream) RecvMsg(m any) error {
	response, err := s.recv()
	if err != nil {
		return err
	}
	defer putmbuf(response.Payload)
	code := codes.Code(response.Header.Code)
	if code != codes.OK {
		return status.Error(code, response.Header.Desc)
	}
	if err := s.codec.Unmarshal(response.Payload, m); err != nil {
		return err
	}
	return nil
}

func (s *clientStream) Context() context.Context {
	return s.ctx
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *clientStream) send(p ...[]byte) error {
	var header api.Header
	for _, opt := range s.opts {
		opt.apply(&header)
	}
	opt := CallOptions{
		Capacity: uint32(len(p)),
	}
	opt.apply(&header)
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	buffers := append(net.Buffers{h}, p...)
	return s.channel.Send(s.id, buffers...)
}

func (s *clientStream) put(h *api.Header) error {
	if ok := h.Closed; ok {
		return s.Close()
	}
	if h.Capacity == 0 {
		select {
		case s.c <- &api.Response{Header: h}:
		default:
			return s.Close()
		}
		return nil
	}
	for range h.Capacity {
		streamID, p, err := s.channel.Recv()
		if err != nil {
			return err
		}
		if streamID != s.id {
			putmbuf(p)
			return status.DataLoss.Err()
		}
		select {
		case s.c <- &api.Response{Header: h, Payload: p}:
		default:
			putmbuf(p)
			return s.Close()
		}
	}
	return nil
}

// recv receives a message from the stream. It blocks until a message is received or the context is canceled. If the stream is closed, an error is returned.
func (s *clientStream) recv() (*api.Response, error) {
	select {
	case <-s.ctx.Done():
		return nil, status.Canceled.Err()
	case response, ok := <-s.c:
		if !ok {
			return nil, status.Unavailable.Err()
		}
		return response, nil
	}
}

type CallOption interface {
	apply(*api.Header)
}

type CallOptions api.Header

func (c *CallOptions) apply(opts *api.Header) {
	if c.Timeout > 0 && c.Timeout < opts.Timeout {
		opts.Timeout = c.Timeout
	}
	opts.Method = cmp.Or(c.Method, opts.Method)
	opts.Service = cmp.Or(c.Service, opts.Service)
	opts.Capacity = cmp.Or(c.Capacity, opts.Capacity)
	opts.Authority = cmp.Or(c.Authority, opts.Authority)
	opts.Closed = cmp.Or(c.Closed, opts.Closed)
	opts.Ping = cmp.Or(c.Ping, opts.Ping)
	opts.Code = cmp.Or(c.Code, opts.Code)
	opts.Desc = cmp.Or(c.Desc, opts.Desc)
	opts.Metadatas = append(opts.Metadatas, c.Metadatas...)
}

type unaryStream struct {
	grpc.ServerStream
	ctx      context.Context
	id       uint32
	codec    encoding.Codec
	channel  *channel
	capacity uint32
}

func (s *unaryStream) Context() context.Context {
	return s.ctx
}

func (s *unaryStream) send(opts ...CallOption) error {
	var header api.Header
	for _, v := range opts {
		v.apply(&header)
	}
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	return s.channel.Send(s.id, h)
}

func (s *unaryStream) SendMsg(m any) error {
	b, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	var header api.Header
	opt := CallOptions{Code: uint32(codes.OK), Capacity: 1}
	opt.apply(&header)
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	buffers := net.Buffers{h, b}
	return s.channel.Send(s.id, buffers...)
}

func (s *unaryStream) RecvMsg(m any) error {
	if s.capacity == 0 {
		return status.OutOfRange.Err()
	}
	s.capacity--
	streamID, p, err := s.channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(p)
	if streamID != s.id {
		return status.DataLoss.Err()
	}
	if err := s.codec.Unmarshal(p, m); err != nil {
		return err
	}
	return nil
}

func (s *unaryStream) Close() error {
	for n := s.capacity; n > 0; n-- {
		_, p, err := s.channel.Recv()
		if err != nil {
			return err
		}
		putmbuf(p)
	}
	return nil
}

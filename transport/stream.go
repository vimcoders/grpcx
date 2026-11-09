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
	"sync"
	"sync/atomic"

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
	sync.Once
}

func (s *clientStream) CloseSend() error {
	defer s.Close()
	s.Once.Do(func() {
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
		// :: TODO
		// opt := CallOptions{Closed: true}
		// _ = s.Send(&api.Empty{}, &opt)
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

func (s *clientStream) Recv(m any) error {
	reply, err := s.recv()
	if err != nil {
		return err
	}
	defer putmbuf(reply.Payload)
	code := codes.Code(reply.Header.Code)
	if code != codes.OK {
		return status.Error(code, reply.Header.Desc)
	}
	if err := s.codec.Unmarshal(reply.Payload, m); err != nil {
		return err
	}
	return nil
}

func (s *clientStream) Context() context.Context {
	return s.ctx
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *clientStream) Send(m any, opts ...CallOption) error {
	var header api.Header
	for _, v := range opts {
		v.apply(&header)
	}
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	b, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	return s.send(s.id, h, b)
}

func (s *clientStream) invoke(ctx context.Context, opts ...CallOption) error {
	var h api.Header
	for _, v := range opts {
		v.apply(&h)
	}
	streamID, p, err := s.channel.Recv()
	if err != nil {
		return err
	}
	if s.id != streamID {
		putmbuf(p)
		return status.DataLoss.Err()
	}
	select {
	case s.c <- &api.Response{Header: &h, Payload: p}:
		return nil
	case <-ctx.Done():
	case <-s.ctx.Done():
	default:
	}
	putmbuf(p)
	return s.Close()
}

func (s *clientStream) send(streamID uint32, p ...[]byte) error {
	return s.channel.Send(streamID, p...)
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
	if c.Timeout > 0 {
		opts.Timeout = min(cmp.Or(opts.Timeout, c.Timeout), c.Timeout)
	}
	opts.Method = cmp.Or(c.Method, opts.Method)
	opts.Service = cmp.Or(c.Service, opts.Service)
	opts.Length = cmp.Or(c.Length, opts.Length)
	opts.Authority = cmp.Or(c.Authority, opts.Authority)
	opts.Closed = cmp.Or(c.Closed, opts.Closed)
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
	consumed atomic.Bool
}

func (s *unaryStream) Context() context.Context {
	return s.ctx
}

func (s *unaryStream) Send(m any, opts ...CallOption) error {
	var header api.Header
	for _, v := range opts {
		v.apply(&header)
	}
	h, err := encoding.Marshal(&header)
	if err != nil {
		return err
	}
	b, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	return s.channel.Send(s.id, h, b)
}

func (s *unaryStream) Recv(m any) error {
	if s.consumed.Swap(true) {
		return status.DataLoss.Err()
	}
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
	if s.consumed.Swap(true) {
		return status.DataLoss.Err()
	}
	if _, err := s.channel.discard(); err != nil {
		return err
	}
	return nil
}

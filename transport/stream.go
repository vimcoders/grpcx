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
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type clientStream struct {
	Sender
	grpc.ClientStream
	header       *api.Header
	ctx          context.Context
	id           uint32
	c            chan *api.Response
	cancel       context.CancelFunc
	deleteStream func(id uint32)
	codec        encoding.Codec
	sync.Once
}

func (s *clientStream) CloseSend() error {
	s.Once.Do(func() {
		defer s.Close()
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
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
	return s.send(&api.Request{
		Header:  s.header,
		Payload: p,
	})
}

func (s *clientStream) RecvMsg(m any) error {
	response, err := s.recv()
	if err != nil {
		return err
	}
	defer putmbuf(response.Payload)
	code := codes.Code(response.Header.Code)
	if code != codes.OK {
		return status.Error(code, response.Header.Message)
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
func (s *clientStream) send(req *api.Request) error {
	if len(req.Payload) > messageLengthMax {
		return status.OutOfRange.Err()
	}
	h, err := encoding.Marshal(req.Header)
	if err != nil {
		return err
	}
	if len(h) > messageLengthMax {
		return status.OutOfRange.Err()
	}
	return s.Sender.Send(s.id, h, req.Payload)
}

func (s *clientStream) put(header *api.Header, p []byte) error {
	select {
	case s.c <- &api.Response{Header: header, Payload: p}:
		return nil
	default:
		defer putmbuf(p)
		return s.CloseSend()
	}
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
	opts.Timeout = cmp.Or(c.Timeout, opts.Timeout)
	opts.Method = cmp.Or(c.Method, opts.Method)
	opts.Service = cmp.Or(c.Service, opts.Service)
	opts.Metadatas = append(opts.Metadatas, c.Metadatas...)
}

type serverStream struct {
	Sender
	grpc.ServerStream
	ctx    context.Context
	id     uint32
	p      []byte
	header *api.Header
	codec  encoding.Codec
}

func (s *serverStream) Context() context.Context {
	return s.ctx
}

func (s *serverStream) senderr(code codes.Code, message string) error {
	header := s.header
	header.Code = uint32(code)
	header.Message = message
	resp := &api.Response{
		Header: header,
	}
	return s.send(resp)
}

func (s *serverStream) send(resp *api.Response) error {
	if len(resp.Payload) > messageLengthMax {
		return status.OutOfRange.Err()
	}
	h, err := encoding.Marshal(resp.Header)
	if err != nil {
		return err
	}
	return s.Sender.Send(s.id, h, resp.Payload)
}

func (s *serverStream) SendMsg(m any) error {
	b, err := s.codec.Marshal(m)
	if err != nil {
		return err
	}
	header := s.header
	header.Code = uint32(codes.OK)
	resp := &api.Response{
		Header:  header,
		Payload: b,
	}
	return s.send(resp)
}

func (s *serverStream) RecvMsg(m any) error {
	if err := s.codec.Unmarshal(s.p, m); err != nil {
		return err
	}
	return nil
}

func (s *serverStream) Timeout() time.Duration {
	if s.header == nil {
		return 0
	}
	return time.Millisecond * time.Duration(s.header.Timeout)
}

func (s *serverStream) Method() string {
	if s.header == nil {
		return ""
	}
	return s.header.Method
}

type ServerStream interface {
	grpc.ServerStream
}

func readCall(channel *channel) (uint32, *api.Header, []byte, error) {
	hdrID, h, err := channel.Recv()
	if err != nil {
		return 0, nil, nil, err
	}
	defer putmbuf(h)
	payloadID, p, err := channel.Recv()
	if err != nil {
		return 0, nil, nil, err
	}
	if payloadID != hdrID {
		putmbuf(p)
		return 0, nil, nil, status.DataLoss.Err()
	}
	var header api.Header
	if err := encoding.Unmarshal(h, &header); err != nil {
		putmbuf(p)
		return 0, nil, nil, err
	}
	return hdrID, &header, p, nil
}

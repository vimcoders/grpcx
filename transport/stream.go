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
	"io"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type clientStream struct {
	grpc.ClientStream
	CallOptions
	ctx          context.Context
	id           uint32
	s            Sender
	c            chan *api.Response
	Cancel       context.CancelFunc
	deleteStream func(id uint32)
	sync.Once
}

func (s *clientStream) CloseSend() error {
	s.Once.Do(func() {
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
	})
	return s.Close()
}

// close closes the stream and releases any resources associated with it.
func (s *clientStream) Close() error {
	return nil
}

func (s *clientStream) SendMsg(m any) error {
	b, err := s.Marshal(m)
	if err != nil {
		return err
	}
	header := &api.Header{
		Method:  s.Method,
		Timeout: s.Timeout.Milliseconds(),
	}
	for k, v := range s.Metadatas {
		header.Metadatas = append(header.Metadatas, k, v)
	}
	return s.send(&api.Request{
		Header:  header,
		Payload: b,
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
	if err := s.Unmarshal(response.Payload, m); err != nil {
		return err
	}
	return nil
}

func (s *clientStream) Context() context.Context {
	return s.ctx
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *clientStream) send(req *api.Request) error {
	h, err := encoding.Marshal(req.Header)
	if err != nil {
		return err
	}
	return s.s.Send(s.id, h, req.Payload)
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
	apply(*CallOptions)
}

var defaultCallOptions = CallOptions{
	Timeout: defaultTimeout,
	Codec:   encoding.GetCodec(encoding.Name),
}

type CallOptions struct {
	Method    string
	Timeout   time.Duration
	Metadatas metadata.MD
	encoding.Codec
}

func (c CallOptions) apply(opts *CallOptions) {
	if c.Codec != nil {
		opts.Codec = c.Codec
	}
	if len(c.Method) > 0 {
		opts.Method = c.Method
	}
	if c.Timeout > 0 && c.Timeout < opts.Timeout {
		opts.Timeout = c.Timeout
	}
	if len(c.Metadatas) > 0 {
		opts.Metadatas = c.Metadatas
	}
}

type serverStream struct {
	grpc.ServerStream
	ctx context.Context
	id  uint32
	s   Sender
	p   []byte
	CallOptions
}

func (s *serverStream) Context() context.Context {
	return s.ctx
}

func (s *serverStream) send(resp *api.Response) error {
	h, err := encoding.Marshal(resp.Header)
	if err != nil {
		return err
	}
	return s.s.Send(s.id, h, resp.Payload)
}

func (s *serverStream) SendMsg(m any) error {
	b, err := s.Marshal(m)
	if err != nil {
		return err
	}
	header := &api.Header{
		Code:    int32(codes.OK),
		Timeout: s.Timeout.Milliseconds(),
		Method:  s.Method,
	}
	for k, v := range s.Metadatas {
		header.Metadatas = append(header.Metadatas, k, v)
	}
	resp := &api.Response{
		Header:  header,
		Payload: b,
	}
	return s.send(resp)
}

func (s *serverStream) RecvMsg(m any) error {
	if err := s.Unmarshal(s.p, m); err != nil {
		return err
	}
	return nil
}

func (s *serverStream) Close() error {
	defer putmbuf(s.p)
	return nil
}

type ServerStream interface {
	grpc.ServerStream
	io.Closer
}

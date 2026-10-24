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

package roundtrip

import (
	"context"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/codes"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type stream struct {
	ctx context.Context
	id  uint32
	s   Sender
	c   chan *api.Response
	CallOptions
}

// newStream creates a new stream with the given id and sender.
func newStream(ctx context.Context, id uint32, send Sender, opts ...CallOption) *stream {
	options := defaultCallOptions
	for _, opt := range opts {
		opt.apply(&options)
	}
	if dl, ok := ctx.Deadline(); ok {
		opt := CallOptions{
			Timeout: time.Until(dl),
		}
		opt.apply(&options)
	}
	return &stream{
		ctx:         ctx,
		id:          id,
		s:           send,
		c:           make(chan *api.Response, 1),
		CallOptions: options,
	}
}

// close closes the stream and releases any resources associated with it.
func (s *stream) Close() error {
	if s.deleteStream != nil {
		s.deleteStream(s.id)
	}
	return nil
}

func (s *stream) SendMsg(m any) error {
	b, err := s.Marshal(m)
	if err != nil {
		return err
	}
	req := &api.Request{
		Method:  s.Method,
		Timeout: s.Timeout.Milliseconds(),
		Payload: b,
	}
	for k, v := range s.Metadatas {
		req.Metadatas = append(req.Metadatas, k, v)
	}
	return s.send(req)
}

func (s *stream) RecvMsg(m any) error {
	response, err := s.recv()
	if err != nil {
		return err
	}
	code := codes.Code(response.Code)
	if code != codes.OK {
		return status.Error(code, response.Message)
	}
	if err := s.Unmarshal(response.Payload, m); err != nil {
		return err
	}
	return nil
}

func (s *stream) Context() context.Context {
	return s.ctx
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *stream) send(req *api.Request) error {
	b, err := encoding.Marshal(req)
	if err != nil {
		return err
	}
	return s.s.Send(s.id, b)
}

// receive receives a message from the stream. The message is received with a fixed-length header that includes the stream id. If the stream is closed, an error is returned.
func (s *stream) process(ctx context.Context, b []byte) error {
	var response api.Response
	if err := encoding.Unmarshal(b, &response); err != nil {
		return err
	}
	select {
	case s.c <- &response:
		return nil
	case <-s.ctx.Done():
		return s.Close()
	case <-ctx.Done():
		return s.Close()
	default:
		return s.Close()
	}
}

// recv receives a message from the stream. It blocks until a message is received or the context is canceled. If the stream is closed, an error is returned.
func (s *stream) recv() (*api.Response, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.Timeout)
	defer cancel()
	select {
	case <-ctx.Done():
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
	deleteStream func(id uint32)
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
	if c.deleteStream != nil {
		opts.deleteStream = c.deleteStream
	}
}

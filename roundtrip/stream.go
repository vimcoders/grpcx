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

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/codes"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type stream struct {
	ctx  context.Context
	id   uint32
	s    Sender
	c    chan *api.Response
	opts []CallOption
}

// newStream creates a new stream with the given id and sender.
func newStream(ctx context.Context, id uint32, send Sender, opts ...CallOption) *stream {
	return &stream{
		ctx:  ctx,
		id:   id,
		s:    send,
		c:    make(chan *api.Response, 1),
		opts: opts,
	}
}

// close closes the stream and releases any resources associated with it.
func (s *stream) close() error {
	return nil
}

func (s *stream) SendMsg(m any) error {
	req := &api.Request{}
	for _, opt := range s.opts {
		if err := opt.before(req, m); err != nil {
			return err
		}
	}
	return s.send(s.Context(), req)
}

func (s *stream) RecvMsg(m any) error {
	response, err := s.recv(s.Context())
	if err != nil {
		return err
	}
	code := codes.Code(response.Code)
	if code != codes.OK {
		return status.Error(code, response.Message)
	}
	for _, opt := range s.opts {
		if err := opt.after(response, m); err != nil {
			return err
		}
	}
	return nil
}

func (s *stream) Context() context.Context {
	return s.ctx
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *stream) send(_ context.Context, req *api.Request) error {
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
		return s.close()
	case <-ctx.Done():
		return s.close()
	default: // 上一个响应还没被取走，属于异常帧，丢弃
		return nil
	}
}

// recv receives a message from the stream. It blocks until a message is received or the context is canceled. If the stream is closed, an error is returned.
func (s *stream) recv(ctx context.Context) (*api.Response, error) {
	select {
	case <-ctx.Done():
		return nil, status.Canceled.Err()
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
	// before is called before sending the request. It can be used to modify the request or add metadata.
	before(*api.Request, any) error
	// after is called after receiving the response. It can be used to modify the response or extract metadata.
	after(*api.Response, any) error
}

type CallOptions struct {
	Method   string
	Timeout  int64
	Metadata metadata.MD
	codec    encoding.Codec
}

func (c *CallOptions) before(r *api.Request, m any) error {
	if c.Method != "" {
		r.Method = c.Method
	}
	if c.Timeout > 0 {
		r.Timeout = c.Timeout
	}
	for k, v := range c.Metadata {
		r.Metadatas = append(r.Metadatas, k, v)
	}
	if c.codec != nil {
		b, err := c.codec.Marshal(m)
		if err != nil {
			return err
		}
		r.Payload = b
	}
	return nil
}

func (c *CallOptions) after(r *api.Response, m any) error {
	if c.codec != nil {
		return c.codec.Unmarshal(r.Payload, m)
	}
	return nil
}

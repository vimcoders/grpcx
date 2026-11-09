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

// clientStream is the client-side state of one unary call: the stream id, the
// channel that carries it, the receive slot and the cleanup hooks. The
// embedded grpc.ClientStream is never assigned and exists only to satisfy the
// grpc.ClientStream interface.
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

// CloseSend deregisters the stream exactly once, then closes it. The end-of-
// stream frame is intentionally not sent: grpcx only carries unary calls, so
// there is no stream to terminate.
func (s *clientStream) CloseSend() error {
	defer s.Close()
	s.Once.Do(func() {
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
		// TODO: send the Closed frame once streaming lands.
		// opt := CallOptions{Closed: true}
		// _ = s.Send(&api.Empty{}, &opt)
	})
	return nil
}

// Close cancels the stream context, unblocking any pending recv.
func (s *clientStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Recv waits for the reply frame, checks its code and unmarshals its payload
// into m.
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

// Context returns the stream context, canceled when the stream is closed.
func (s *clientStream) Context() context.Context {
	return s.ctx
}

// Send marshals the header and the payload into two frames and writes them on
// the stream. The header carries the stream id, so the two frames stay
// addressable by the server.
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

// invoke reads the response payload straight off the channel, verifies the
// stream id, and hands it to the waiting Recv. When the stream is already
// canceled or gone, the payload is returned to the pool and the stream closed.
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

// send forwards the frames to the channel under this stream's id.
func (s *clientStream) send(streamID uint32, p ...[]byte) error {
	return s.channel.Send(streamID, p...)
}

// recv blocks until the response slot is filled, the stream is canceled, or
// the slot is closed because the connection went away.
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

// CallOption mutates the outgoing request header before it is marshalled.
type CallOption interface {
	apply(*api.Header)
}

// CallOptions is the concrete CallOption: each non-zero field overrides its
// counterpart in the header.
type CallOptions api.Header

// apply merges c into opts field by field. Non-zero values in c win, except
// Timeout, where the smaller of the two is kept so a short caller deadline is
// never silently extended.
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

// unaryStream is the server-side view of one request. It carries the id, the
// channel and the single-use flag that guarantees the request payload is
// consumed at most once. The embedded grpc.ServerStream is never assigned and
// exists only to satisfy the interface; grpc.SetHeader and grpc.SetTrailer
// would therefore panic if called.
type unaryStream struct {
	grpc.ServerStream
	ctx      context.Context
	id       uint32
	codec    encoding.Codec
	channel  *channel
	consumed atomic.Bool
}

// Context returns the server-side context, carrying the request metadata.
func (s *unaryStream) Context() context.Context {
	return s.ctx
}

// Send marshals the header and the payload into two frames and writes the
// reply on the channel.
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

// Recv reads the request payload, verifies the stream id and unmarshals it
// into m. It may run at most once per request: a second call reports
// codes.DataLoss instead of reading another frame off the connection.
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

// Close skips whatever part of the request the handler did not read, so the
// next request on the same connection starts at a frame boundary. Closing a
// stream whose payload was already consumed reports codes.DataLoss.
func (s *unaryStream) Close() error {
	if s.consumed.Swap(true) {
		return status.DataLoss.Err()
	}
	if _, err := s.channel.discard(); err != nil {
		return err
	}
	return nil
}

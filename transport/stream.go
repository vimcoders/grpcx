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
	"path"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// clientStream is the client-side state of one unary call: the stream id, the
// channel that carries it, the receive slot and the cleanup hooks. It embeds
// ClientStream so the generated stubs and the interceptor chain see the
// grpc.ClientStream surface they are typed against. Only Send, Recv, Context
// and CloseSend are implemented here: the embedded grpc.ClientStream is never
// assigned, so Header, Trailer, SendMsg and RecvMsg fall through to nil and
// would panic if called.
type clientStream struct {
	ClientStream
	channel      *channel
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

// Send marshals the header and the payload into two frames and writes them to
// the channel under this stream's id. The header carries the id, so the two
// frames stay addressable by the server.
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
	return s.channel.Send(s.id, h, b)
}

// UnaryInterceptor reads the response payload straight off the channel,
// verifies the stream id, and hands it to the waiting Recv. When the stream is
// already canceled or gone, the payload is returned to the pool and the stream
// closed.
func (s *clientStream) UnaryInterceptor(ctx context.Context, opts ...CallOption) error {
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

// WithUnaryInterceptor turns an already decoded request header into a
// CallOption, so the server-side handler sees the call it was dispatched from
// without re-parsing the frame.
func WithUnaryInterceptor(readonly *api.Header) CallOption {
	return &CallOptions{
		Method:    readonly.Method,
		Service:   readonly.Service,
		Length:    readonly.Length,
		Authority: readonly.Authority,
		Timeout:   readonly.Timeout,
		Metadatas: readonly.Metadatas,
		Code:      readonly.Code,
		Desc:      readonly.Desc,
	}
}

// WithUnaryCode sets the response status code and its description, so a reply
// can report a failure instead of a payload.
func WithUnaryCode(code codes.Code, desc string) CallOption {
	return &CallOptions{
		Code: uint32(code),
		Desc: desc,
	}
}

// unary is the server-side view of one request: the stream id, the channel it
// arrived on, the service descriptor to look the method up in, and the
// interceptors to run. It embeds Options so the server-side settings, timeout
// among them, apply to the call, and ServerStream so the handler and the
// interceptor chain see the grpc.ServerStream surface they are typed against.
// Only Send and Recv are implemented here: the embedded grpc.ServerStream is
// never assigned, so SetHeader, SetTrailer and Context fall through to nil and
// would panic if called.
type unary struct {
	ServerStream
	Options
	id               uint32
	codec            encoding.Codec
	channel          *channel
	desc             *grpc.ServiceDesc
	imp              any
	unaryInterceptor grpc.UnaryServerInterceptor
}

// UnaryInterceptor resolves the method from the decoded header, applies the
// metadata and timeout, runs the unary interceptor chain and writes the reply
// back. An unknown method reports codes.Unimplemented.
func (s *unary) UnaryInterceptor(ctx context.Context, opts ...CallOption) error {
	opt := api.Header{Code: uint32(codes.OK), Desc: codes.OK.String()}
	for _, v := range opts {
		v.apply(&opt)
	}
	for _, v := range s.desc.Methods {
		if opt.Method != path.Join("/", s.desc.ServiceName, v.MethodName) {
			continue
		}
		metadatas := metadata.Pairs(opt.Metadatas...)
		timeout := cmp.Or(time.Duration(opt.Timeout)*time.Millisecond, s.timeout, defaultTimeout)
		timeoutCtx, cancel := context.WithTimeout(metadata.WithMetadata(ctx, metadatas), timeout)
		defer cancel()
		reply, err := v.Handler(s.imp, timeoutCtx, s.Recv, s.unaryInterceptor)
		if err != nil {
			return s.Send(&api.Empty{}, WithUnaryCode(status.Code(err), err.Error()))
		}
		return s.Send(reply)
	}
	if err := s.Recv(&api.Empty{}); err != nil {
		return err
	}
	return s.Send(&api.Empty{}, WithUnaryCode(codes.Unimplemented, opt.Method))
}

// Send marshals the header and the payload into two frames and writes the
// reply on the channel.
func (s *unary) Send(m any, opts ...CallOption) error {
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

// Recv reads the request payload, verifies the stream id and unmarshals it into
// m. A unary request carries exactly one payload frame, so the handler calls
// this once; a mismatched id means the frame belongs to another call and
// reports codes.DataLoss.
func (s *unary) Recv(m any) error {
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

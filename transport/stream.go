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
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// reply is one response frame as it comes off the wire: the header options and
// the payload together, because the caller needs the status and the bytes at
// the same point. Buffer is embedded so Free and ReadOnlyData reach whoever
// takes the frame out of the receive slot.
type reply struct {
	CallOption
	Buffer
}

// clientStream is the client-side state of one call: the stream id, the
// connection that carries it, the receive slot and the cleanup hooks. It embeds
// grpc.ClientStream so the generated stubs and the interceptor chain see the
// surface they are typed against.
//
// Only Context and CloseSend are implemented here, plus the internal recv.
// SendMsg, RecvMsg, Header and Trailer fall through to the embedded interface,
// which is never assigned, and would panic if called. Requests are written by
// client.RoundTrip through the embedded Sender, not by a method on this type.
type clientStream struct {
	grpc.ClientStream
	Sender
	ctx          context.Context
	id           uint32
	c            chan *reply
	cancel       context.CancelFunc
	deleteStream func(id uint32)
	sync.Once
}

// CloseSend deregisters the stream exactly once, then closes it. No end-of-
// stream frame is sent: grpcx carries unary calls only, so there is no stream
// to terminate. The once covers deregistration, so a second CloseSend skips the
// delete while Close stays repeatable.
func (s *clientStream) CloseSend() error {
	// CloseSend always reports success, so the close error is only cleanup.
	defer func() { _ = s.Close() }()
	s.Once.Do(func() {
		if s.deleteStream != nil {
			s.deleteStream(s.id)
		}
		// TODO: send an end-of-stream frame once streaming lands and the header
		// grows a flag for it.
	})
	return nil
}

// Close cancels the stream context, waking any pending recv through its
// context branch. The cancel is idempotent, so a repeated Close is harmless.
// The receive slot is left open: a reply that arrives after Close lands in the
// slot instead of panicking a sender, so Handle stays safe whenever the call
// is torn down.
func (s *clientStream) Close() error {
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

// Context returns the stream context, canceled when the stream is closed.
func (s *clientStream) Context() context.Context {
	return s.ctx
}

// Handle reads the response payload frame off the connection and hands it to
// the caller waiting in recv through the receive slot.
//
// A call that has already ended — the stream context is done — drops the
// payload back to the pool, and so does an occupied slot: neither would ever
// be read.
func (s *clientStream) Handle(ctx context.Context, recv Recver, opts ...CallOption) error {
	var opt api.CallOptions
	for _, o := range opts {
		o.apply(&opt)
	}
	p, err := recv.Recv()
	if err != nil {
		return err
	}
	response := &reply{
		CallOption: WithCallOptions(&opt),
		Buffer:     p,
	}
	if s.ctx.Err() != nil {
		defer p.Free()
		return s.Close()
	}
	select {
	case s.c <- response:
		return nil
	default:
		defer p.Free()
		return s.Close()
	}
}

// recv waits for the reply slot or for the call to end.
//
// The code depends on why the context ended, not on the outcome: a canceled
// context reports Canceled, an expired deadline reports Unavailable. Two
// deadlines land on the deadline branch and are not told apart here — the
// caller's own and the transport's read timeout — because the stream context
// cannot say which one fired. Close cancels every live stream while it walks
// the table, which lands here as Canceled too.
//
// The slot is never closed today, so the !ok branch is a guard only: a recv
// parked here is woken by the context, never by the slot.
func (s *clientStream) recv() (*reply, error) {
	select {
	case <-s.ctx.Done():
		if errors.Is(s.ctx.Err(), context.DeadlineExceeded) {
			return nil, status.Unavailable.Err()
		}
		return nil, status.Canceled.Err()
	case response, ok := <-s.c:
		if !ok {
			return nil, status.Canceled.Err()
		}
		return response, nil
	}
}

// CallOption mutates the outgoing request header before it is marshalled.
type CallOption interface {
	apply(*api.CallOptions)
}

// CallOptions is the concrete CallOption: each non-zero field overrides its
// counterpart in the header. Metadatas is the exception — the pairs accumulate
// across the options applied.
type CallOptions api.CallOptions

// apply merges c into opts field by field: a non-zero field in c replaces what
// an earlier option put there, and a zero one leaves it alone. apply never
// fills in a default, so "this header named no timeout" survives to the call
// site: Invoke and RoundTrip fold the context deadline in, and
// defaultMethodHandler.Handle falls back to WithServerTimeout.
func (c *CallOptions) apply(opts *api.CallOptions) {
	opts.Timeout = cmp.Or(c.Timeout, opts.Timeout)
	opts.FullMethodName = cmp.Or(c.FullMethodName, opts.FullMethodName)
	opts.Service = cmp.Or(c.Service, opts.Service)
	opts.Code = cmp.Or(c.Code, opts.Code)
	opts.Desc = cmp.Or(c.Desc, opts.Desc)
	opts.Metadatas = append(opts.Metadatas, c.Metadatas...)
	opts.StreamID = cmp.Or(c.StreamID, opts.StreamID)
}

// decodeCallOptions reads the request header frame off recv and returns the
// header the wire actually carried. The header is decoded with the built-in
// codec of package encoding, not with the codec message bodies use. Callers
// use it for dispatch and hand it to WithCallOptions when the handler should
// see the same metadata and deadline.
func decodeCallOptions(recv Recver) (*api.CallOptions, error) {
	p, err := recv.Recv()
	if err != nil {
		return nil, err
	}
	defer p.Free()
	var header api.CallOptions
	if err := encoding.Unmarshal(p.ReadOnlyData(), &header); err != nil {
		return nil, err
	}
	return &header, nil
}

// WithCallOptions turns an already decoded request header into a CallOption,
// so the handler sees the call it was dispatched from without re-parsing the
// frame.
func WithCallOptions(readonly *api.CallOptions) CallOption {
	return &CallOptions{
		FullMethodName: readonly.FullMethodName,
		Service:        readonly.Service,
		Timeout:        readonly.Timeout,
		Metadatas:      readonly.Metadatas,
		Code:           readonly.Code,
		Desc:           readonly.Desc,
		StreamID:       readonly.StreamID,
	}
}

// defaultMethodHandler is the server-side view of one unary request: the sender
// it replies through, the server Codec, the timeout ceiling, the service
// implementation and the method handler resolved from the descriptor, plus the
// interceptor to run. It carries Codec and timeout as separate fields rather
// than embedding Options; the rest of the transport settings belong to the
// channel, not to a single call.
//
// A unary request is a single payload already off the wire, so the handler is
// called through grpc.MethodHandler with a decode function instead of a
// grpc.ServerStream. The stream id is not a field: it arrives with the call
// options the dispatcher hands in, and is copied into the reply header, so the
// reply lands on the stream the request came from.
type defaultMethodHandler struct {
	Sender
	Codec
	timeout  time.Duration
	imp      any
	unaryInt grpc.UnaryServerInterceptor
	Handler  grpc.MethodHandler
}

// Handle consumes the request payload, applies the call metadata and the
// deadline, runs the unary interceptor chain over the generated method handler
// and writes the reply back as a header frame and a payload frame.
//
// The deadline is the first non-zero of the header timeout, WithServerTimeout
// and defaultTimeout, capped at defaultTimeout: a header that names a timeout
// wins over the option, and the option can only tighten the ceiling. The
// request payload is decoded through the server Codec and freed on return. A
// handler error is reported in the response header's status code instead of
// tearing the connection down; a panic is recovered the same way with
// codes.Internal.
func (d *defaultMethodHandler) Handle(ctx context.Context, recv Recver, opts ...CallOption) error {
	// Nothing below reads Code or Desc back out of options, and the reply status
	// is built separately in info, so both stay zero: zero is codes.OK, which is
	// what a request that names no status of its own is.
	var options api.CallOptions
	for _, v := range opts {
		v.apply(&options)
	}
	p, err := recv.Recv()
	if err != nil {
		return err
	}
	defer p.Free()
	metadatas := metadata.Pairs(options.Metadatas...)
	timeout := min(cmp.Or(time.Duration(options.Timeout)*time.Millisecond, d.timeout, defaultTimeout), defaultTimeout)
	timeoutCtx, cancel := context.WithTimeout(metadata.WithMetadata(ctx, metadatas), timeout)
	defer cancel()
	info := &CallOptions{Code: uint32(codes.OK), StreamID: options.StreamID}
	s := sender{Codec: d.Codec, Sender: d.Sender}
	dec := func(in any) error { return d.Codec.Unmarshal(p, in) }
	defer func() {
		if r := recover(); r != nil {
			info.Code, info.Desc = uint32(codes.Internal), fmt.Sprintf("handler panic: %v", r)
			_ = s.send(&api.Empty{}, info)
		}
	}()
	reply, err := d.Handler(d.imp, timeoutCtx, dec, d.unaryInt)
	if err != nil {
		info.Code, info.Desc = uint32(status.Code(err)), err.Error()
		return s.send(&api.Empty{}, info)
	}
	if timeoutCtx.Err() != nil {
		info.Code, info.Desc = uint32(codes.DeadlineExceeded), timeoutCtx.Err().Error()
		return s.send(&api.Empty{}, info)
	}
	return s.send(reply, info)
}

// empty answers a method the registered service does not expose: an unknown
// name, or a stream method, which grpcx does not serve.
type empty struct {
	Sender
	Codec
}

// Handle still reads the request payload before replying, so the connection
// stays frame-aligned, then sends codes.Unimplemented carrying the requested
// method name.
func (e *empty) Handle(ctx context.Context, recv Recver, opts ...CallOption) error {
	// The status is set on the reply below; this copy of the request header only
	// supplies the stream id and the requested method name.
	var options api.CallOptions
	for _, v := range opts {
		v.apply(&options)
	}
	p, err := recv.Recv()
	if err != nil {
		return err
	}
	defer p.Free()
	s := sender{Codec: e.Codec, Sender: e.Sender}
	code, desc := uint32(codes.Unimplemented), options.FullMethodName
	return s.send(&api.Empty{}, &CallOptions{Code: code, Desc: desc, StreamID: options.StreamID})
}

// sender writes one message onto the connection as a pair of frames: the
// marshalled call options as the header, the marshalled message as the payload.
// The client uses it to send a request, the server to send a reply, which is
// why it carries neither verb in its shape.
type sender struct {
	Sender
	Codec
}

// send writes m as two frames: the call options as the header, then m as the
// payload, in that order, so the peer stays on a frame boundary. Both are
// marshalled through the embedded Codec and both are freed when send returns.
// The stream id travels inside opts; a zero id there leaves the frame
// unroutable.
//
// Freeing on return is only safe when the Codec returns unpooled bytes, which
// is what codec does. With a defaultCodec under batching, a proto header or
// body would go back to the pool while its bytes were still in the batch.
func (s *sender) send(m any, opts ...CallOption) error {
	options := api.CallOptions{}
	for _, v := range opts {
		v.apply(&options)
	}
	h, err := s.Marshal(&options)
	if err != nil {
		return err
	}
	defer h.Free()
	b, err := s.Marshal(m)
	if err != nil {
		return err
	}
	defer b.Free()
	return s.Sender.Send(h.ReadOnlyData(), b.ReadOnlyData())
}

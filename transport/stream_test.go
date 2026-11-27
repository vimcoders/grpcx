package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// unaryEcho is the grpc.MethodHandler shape a generated service method has: it
// decodes the request, optionally runs the interceptor, and answers the echo
// service's reply.
func unaryEcho(imp any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	var req api.EchoRequest
	if err := dec(&req); err != nil {
		return nil, err
	}
	service := imp.(api.EchoServiceServer)
	call := func(ctx context.Context, req any) (any, error) {
		return service.Echo(ctx, req.(*api.EchoRequest))
	}
	if interceptor != nil {
		return interceptor(ctx, &req, &grpc.UnaryServerInfo{FullMethod: "/api.EchoService/Echo"}, call)
	}
	return call(ctx, &req)
}

// replyHeader decodes the header frame the sender wrote, so a test can compare
// the status the client would read.
func replyHeader(t *testing.T, frames [][]byte) *api.CallOptions {
	t.Helper()
	if len(frames) != 2 {
		t.Fatalf("sender wrote %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal reply header: %v", err)
	}
	return &header
}

// TestCallOptionsApplyLetsNonZeroFieldsWin pins the merge rule: a field the
// caller set replaces what the previous option put there, and a field it left
// zero keeps the earlier value. Timeout is left untouched at zero when nobody
// named one: the default is filled in at the call site (createStream, Invoke,
// RoundTrip) or by the server, so "the header named no timeout" stays visible.
func TestCallOptionsApplyLetsNonZeroFieldsWin(t *testing.T) {
	opts := api.CallOptions{
		FullMethodName: "/old",
		Service:        "old",
		Code:           7,
		Metadatas:      []string{"a", "1"},
	}
	c := CallOptions{
		FullMethodName: "/new",
		Metadatas:      []string{"b", "2"},
	}
	c.apply(&opts)

	if opts.FullMethodName != "/new" {
		t.Errorf("FullMethodName = %q, want /new", opts.FullMethodName)
	}
	if opts.Service != "old" {
		t.Errorf("Service = %q, want the untouched old value", opts.Service)
	}
	if opts.Code != 7 {
		t.Errorf("Code = %d, want the untouched 7", opts.Code)
	}
	if got := strings.Join(opts.Metadatas, ","); got != "a,1,b,2" {
		t.Errorf("Metadatas = %q, want accumulated pairs", got)
	}
	if opts.Timeout != 0 {
		t.Errorf("Timeout = %d, want it left at 0 so the call site can tell nobody named one", opts.Timeout)
	}
}

// TestCallOptionsApplyKeepsAnExplicitTimeout documents that an explicit timeout
// wins over the default, which is what a caller deadline is carried by.
func TestCallOptionsApplyKeepsAnExplicitTimeout(t *testing.T) {
	opts := api.CallOptions{}
	(&CallOptions{Timeout: 250}).apply(&opts)
	if opts.Timeout != 250 {
		t.Errorf("Timeout = %d, want 250", opts.Timeout)
	}
}

// TestWithCallOptionsCopiesTheWireHeader proves the option carries every field
// the dispatcher read off the wire.
func TestWithCallOptionsCopiesTheWireHeader(t *testing.T) {
	header := &api.CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		Service:        "api.EchoService",
		Timeout:        1500,
		Metadatas:      []string{"k", "v"},
		Code:           uint32(codes.InvalidArgument),
		Desc:           "bad argument",
		StreamID:       11,
	}
	var opts api.CallOptions
	WithCallOptions(header).apply(&opts)

	if opts.FullMethodName != header.FullMethodName ||
		opts.Service != header.Service ||
		opts.Timeout != header.Timeout ||
		opts.Code != header.Code ||
		opts.Desc != header.Desc ||
		opts.StreamID != header.StreamID {
		t.Errorf("applied header = (method %q, service %q, code %d, desc %q, timeout %d, stream %d), want (method %q, service %q, code %d, desc %q, timeout %d, stream %d)",
			opts.FullMethodName, opts.Service, opts.Code, opts.Desc, opts.Timeout, opts.StreamID,
			header.FullMethodName, header.Service, header.Code, header.Desc, header.Timeout, header.StreamID)
	}
	if strings.Join(opts.Metadatas, ",") != "k,v" {
		t.Errorf("Metadatas = %q, want k,v", opts.Metadatas)
	}
}

// TestClientStreamSendAppliesTheStreamIDLast covers routing: the id the stream
// knows wins over whatever the caller passed, so a request cannot be sent onto
// another stream.
func TestClientStreamSendAppliesTheStreamIDLast(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.Send(&api.EchoRequest{Message: "hi"}, &CallOptions{StreamID: 99, FullMethodName: "/m"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("Send wrote %d frames, want a header and a payload", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.StreamID != 7 {
		t.Errorf("StreamID = %d, want the stream's own 7", header.StreamID)
	}
	if header.FullMethodName != "/m" {
		t.Errorf("FullMethodName = %q, want /m", header.FullMethodName)
	}
	var req api.EchoRequest
	if err := encoding.Unmarshal(frames[1], &req); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if req.Message != "hi" {
		t.Errorf("payload Message = %q, want hi", req.Message)
	}
}

// TestClientStreamSendMsgCarriesNoExtraOptions covers the grpc.ClientStream
// shape of Send: the header carries the stream id and nothing else.
func TestClientStreamSendMsgCarriesNoExtraOptions(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.SendMsg(&api.EchoRequest{Message: "plain"}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("SendMsg wrote %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.StreamID != 7 || header.FullMethodName != "" || header.Timeout != 0 {
		t.Errorf("header = (method %q, timeout %d, stream %d), want only StreamID 7", header.FullMethodName, header.Timeout, header.StreamID)
	}
}

// TestClientStreamRecvMsgReportsTheStatusCode covers a failed call: the status
// travels in the response header and RecvMsg turns it back into an error without
// touching the caller's message.
func TestClientStreamRecvMsgReportsTheStatusCode(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &reply{
		CallOption: WithCallOptions(&api.CallOptions{Code: uint32(codes.InvalidArgument), Desc: "bad argument"}),
		Buffer:     pooledPayload([]byte("not used")),
	}

	var reply api.EchoResponse
	err := s.RecvMsg(&reply)
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("RecvMsg = %v (%v), want InvalidArgument", err, code)
	}
	if !strings.Contains(err.Error(), "bad argument") {
		t.Errorf("RecvMsg error = %v, want the description carried in the header", err)
	}
	if reply.Message != "" {
		t.Errorf("reply.Message = %q, want it left alone on a failed call", reply.Message)
	}
}

// TestClientStreamRecvMsgUnmarshalsTheReply covers the success path.
func TestClientStreamRecvMsgUnmarshalsTheReply(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &reply{
		CallOption: WithCallOptions(&api.CallOptions{Code: uint32(codes.OK)}),
		Buffer:     payloadFrame(t, &api.EchoResponse{Message: "hi!"}),
	}

	var reply api.EchoResponse
	if err := s.RecvMsg(&reply); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestClientStreamRecvMsgFreesTheReply covers the ownership on the client
// stream: RecvMsg takes the frame out of the slot and gives the payload back
// before it returns, whatever the caller does with the message afterwards.
func TestClientStreamRecvMsgFreesTheReply(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	buf := countingPayload(mustMarshal(t, &api.EchoResponse{Message: "hi!"}))
	s.c <- &reply{
		CallOption: WithCallOptions(&api.CallOptions{Code: uint32(codes.OK)}),
		Buffer:     buf,
	}

	var got api.EchoResponse
	if err := s.RecvMsg(&got); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if got.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", got.Message)
	}
	if buf.freeCount() != 1 {
		t.Errorf("the reply buffer was freed %d times, want 1", buf.freeCount())
	}
}

// TestClientStreamRecvReportsCanceledWhenTheCallContextIsCanceled covers a call
// the caller canceled before its reply arrived: the stream context reports
// context.Canceled, so the recv reports Canceled while a deadline reports
// Unavailable.
func TestClientStreamRecvReportsCanceledWhenTheCallContextIsCanceled(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.cancel()

	var reply api.EchoResponse
	err := s.RecvMsg(&reply)
	if code := status.Code(err); code != codes.Canceled {
		t.Errorf("RecvMsg on a canceled stream = %v (%v), want Canceled", err, code)
	}
}

// TestClientStreamRecvReportsCanceledWhenTheSlotIsClosed covers the guard on a
// closed receive slot. Nothing closes the slot today — Close cancels the stream
// context instead — so the case pins the code a future closer would produce
// rather than a path a caller can reach.
func TestClientStreamRecvReportsCanceledWhenTheSlotIsClosed(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	close(s.c)

	var reply api.EchoResponse
	err := s.RecvMsg(&reply)
	if code := status.Code(err); code != codes.Canceled {
		t.Errorf("RecvMsg on a closed slot = %v (%v), want Canceled", err, code)
	}
}

// TestClientStreamCloseSendDeregistersTheStreamOnce covers the once: a second
// CloseSend skips the delete instead of removing an entry that may already belong
// to a new stream, and the context is done either way.
func TestClientStreamCloseSendDeregistersTheStreamOnce(t *testing.T) {
	s, deleted := newStreamUnderTest(&fakeSender{})
	if err := s.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("second CloseSend: %v", err)
	}
	if deleted.Load() != 1 {
		t.Errorf("deleteStream called %d times, want 1", deleted.Load())
	}
	if s.Context().Err() == nil {
		t.Error("the stream context is still alive after CloseSend")
	}
}

// TestClientStreamCloseIsRepeatable covers the no-once Close: a cancel is
// idempotent, so calling Close twice is harmless and the second call neither
// panics nor changes the outcome.
func TestClientStreamCloseIsRepeatable(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if s.Context().Err() == nil {
		t.Error("the stream context is still alive after Close")
	}
}

// TestClientStreamCloseDropsLateReplies covers a reply that arrives after the
// call ended: Handle sees the done stream context and returns the payload to the
// pool instead of parking it in the slot, so nothing is left for a reader that
// will never come.
func TestClientStreamCloseDropsLateReplies(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := s.Handle(context.Background(), &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoResponse{Message: "late"})}}); err != nil {
		t.Fatalf("Handle after Close: %v", err)
	}
	if len(s.c) != 0 {
		t.Errorf("late reply left in the slot (%d), want dropped", len(s.c))
	}
}

// TestClientStreamHandleDeliversTheResponse covers the receive path the client
// loop uses: the payload frame lands in the stream's slot with the status the
// reply header carried.
func TestClientStreamHandleDeliversTheResponse(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	body := payloadFrame(t, &api.EchoResponse{Message: "hi!"})
	recv := &fakeRecver{frames: []Buffer{body}}

	if err := s.Handle(context.Background(), recv, &CallOptions{Code: uint32(codes.OK)}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	select {
	case r := <-s.c:
		defer r.Free()
		var header api.CallOptions
		r.apply(&header)
		if header.Code != uint32(codes.OK) {
			t.Errorf("reply Code = %d, want OK", header.Code)
		}
		if len(r.ReadOnlyData()) == 0 {
			t.Error("reply payload is empty")
		}
	default:
		t.Fatal("Handle returned without filling the receive slot")
	}
}

// TestClientStreamHandleClosesTheStreamWhenTheCallIsOver covers a late reply:
// when the slot is full or the call already ended, the payload is still consumed
// and given back to the pool, and the stream is closed so the frame stays on a
// boundary.
func TestClientStreamHandleClosesTheStreamWhenTheCallIsOver(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &reply{Buffer: pooledPayload(nil)} // slot is full
	dropped := countingPayload(mustMarshal(t, &api.EchoResponse{Message: "late"}))
	recv := &fakeRecver{frames: []Buffer{dropped}}

	if err := s.Handle(context.Background(), recv); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if s.Context().Err() == nil {
		t.Error("Handle did not close the stream after dropping a late reply")
	}
	if dropped.freeCount() != 1 {
		t.Errorf("the dropped payload was freed %d times, want 1", dropped.freeCount())
	}
}

// TestDecodeCallOptionsReturnsTheWireHeader covers the dispatcher's read: the
// header frame is consumed and parsed.
func TestDecodeCallOptionsReturnsTheWireHeader(t *testing.T) {
	want := &api.CallOptions{FullMethodName: "/api.EchoService/Echo", StreamID: 3, Timeout: 250}
	header, err := decodeCallOptions(&fakeRecver{frames: []Buffer{headerFrame(t, want)}})
	if err != nil {
		t.Fatalf("decodeCallOptions: %v", err)
	}
	if header.FullMethodName != want.FullMethodName || header.StreamID != want.StreamID || header.Timeout != want.Timeout {
		t.Errorf("header = (method %q, stream %d, timeout %d), want (method %q, stream %d, timeout %d)",
			header.FullMethodName, header.StreamID, header.Timeout,
			want.FullMethodName, want.StreamID, want.Timeout)
	}
}

// TestDecodeCallOptionsReportsAFinishedConnection covers a connection that ends
// before the header arrives.
func TestDecodeCallOptionsReportsAFinishedConnection(t *testing.T) {
	if _, err := decodeCallOptions(&fakeRecver{}); !errors.Is(err, io.EOF) {
		t.Errorf("decodeCallOptions on an empty connection = %v, want io.EOF", err)
	}
}

// TestSenderSendUsesTheArgumentStreamID covers the reply path: the stream id the
// sender is given wins over the one inside the options.
func TestSenderSendUsesTheArgumentStreamID(t *testing.T) {
	fs := &fakeSender{}
	s := sender{Codec: encoding.GetCodec(encoding.Name), Sender: fs}
	if err := s.send(42, &api.Empty{}, &CallOptions{StreamID: 99}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if header := replyHeader(t, fs.sent()); header.StreamID != 42 {
		t.Errorf("StreamID = %d, want the argument's 42", header.StreamID)
	}
}

// TestDefaultMethodHandlerRepliesOK covers the success path: the handler runs
// against the decoded request and its reply goes back on the request's stream.
func TestDefaultMethodHandlerRepliesOK(t *testing.T) {
	fs := &fakeSender{}
	d := &defaultMethodHandler{Sender: fs, Options: DefaultOptions(), imp: &echoImpl{}, Handler: unaryEcho}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "hi"})}}

	if err := d.Handle(context.Background(), recv, &CallOptions{StreamID: 5}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	frames := fs.sent()
	header := replyHeader(t, frames)
	if header.Code != uint32(codes.OK) || header.StreamID != 5 {
		t.Errorf("reply header = %+v, want Code OK on stream 5", header)
	}
	var reply api.EchoResponse
	if err := encoding.Unmarshal(frames[1], &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestDefaultMethodHandlerReportsAHandlerError covers a handler that fails: the
// status travels in the reply header, and Handle itself reports success so the
// connection stays up.
func TestDefaultMethodHandlerReportsAHandlerError(t *testing.T) {
	fs := &fakeSender{}
	d := &defaultMethodHandler{Sender: fs, Options: DefaultOptions(), imp: &echoImpl{failOn: "boom"}, Handler: unaryEcho}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "boom"})}}

	if err := d.Handle(context.Background(), recv, &CallOptions{StreamID: 2}); err != nil {
		t.Fatalf("Handle returned %v, want nil (the error belongs in the reply header)", err)
	}
	frames := fs.sent()
	header := replyHeader(t, frames)
	if codes.Code(header.Code) != codes.InvalidArgument {
		t.Errorf("reply Code = %v, want InvalidArgument", codes.Code(header.Code))
	}
	if got := len(frames[1]); got != 0 {
		t.Errorf("reply payload = %d bytes, want the zero-length frame a failed call carries", got)
	}
}

// TestDefaultMethodHandlerCapsTheHandlerDeadline covers the server side of the
// timeout: a client that asks for an hour still hands its handler the
// transport's own limit.
func TestDefaultMethodHandlerCapsTheHandlerDeadline(t *testing.T) {
	fs := &fakeSender{}
	o := DefaultOptions()
	WithTimeout(10 * time.Second)(&o)
	var seen time.Duration
	d := &defaultMethodHandler{
		Sender:  fs,
		Options: o,
		imp:     &echoImpl{},
		Handler: func(_ any, ctx context.Context, _ func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, errors.New("no deadline on the handler context")
			}
			seen = time.Until(deadline)
			return &api.EchoResponse{Message: "ok"}, nil
		},
	}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "hi"})}}

	if err := d.Handle(context.Background(), recv, &CallOptions{StreamID: 1, Timeout: 3_600_000}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if seen > defaultTimeout || seen <= 0 {
		t.Errorf("handler deadline is %v away, want at most the %v cap", seen, defaultTimeout)
	}
}

// TestDefaultMethodHandlerReportsDeadlineExceeded covers a handler that answers
// too late: the reply carries DeadlineExceeded even though the handler returned
// no error.
func TestDefaultMethodHandlerReportsDeadlineExceeded(t *testing.T) {
	fs := &fakeSender{}
	o := DefaultOptions()
	d := &defaultMethodHandler{
		Sender:  fs,
		Options: o,
		imp:     &echoImpl{},
		Handler: func(_ any, _ context.Context, _ func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			time.Sleep(120 * time.Millisecond)
			return &api.EchoResponse{Message: "too late"}, nil
		},
	}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "hi"})}}

	// The handler's deadline comes off the wire, so 40ms is the one that expires.
	if err := d.Handle(context.Background(), recv, &CallOptions{StreamID: 4, Timeout: 40}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	frames := fs.sent()
	header := replyHeader(t, frames)
	if codes.Code(header.Code) != codes.DeadlineExceeded {
		t.Errorf("reply Code = %v, want DeadlineExceeded", codes.Code(header.Code))
	}
	if !strings.Contains(strings.ToLower(header.Desc), "deadline") {
		t.Errorf("reply Desc = %q, want the context deadline error", header.Desc)
	}
	if got := len(frames[1]); got != 0 {
		t.Errorf("reply payload = %d bytes, want the zero-length frame a failed call carries", got)
	}
}

// TestDefaultMethodHandlerRecoversFromAPanickingHandler covers the isolation: a
// panicking handler becomes an Internal reply instead of taking the process down.
func TestDefaultMethodHandlerRecoversFromAPanickingHandler(t *testing.T) {
	fs := &fakeSender{}
	d := &defaultMethodHandler{
		Sender:  fs,
		Options: DefaultOptions(),
		imp:     &echoImpl{},
		Handler: func(any, context.Context, func(any) error, grpc.UnaryServerInterceptor) (any, error) {
			panic("handler boom")
		},
	}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "hi"})}}

	if err := d.Handle(context.Background(), recv, &CallOptions{StreamID: 6}); err != nil {
		t.Fatalf("Handle after a panic = %v, want nil", err)
	}
	frames := fs.sent()
	header := replyHeader(t, frames)
	if codes.Code(header.Code) != codes.Internal {
		t.Errorf("reply Code = %v, want Internal", codes.Code(header.Code))
	}
	if !strings.Contains(header.Desc, "handler boom") {
		t.Errorf("reply Desc = %q, want it to name the panic", header.Desc)
	}
	if got := len(frames[1]); got != 0 {
		t.Errorf("reply payload = %d bytes, want the zero-length frame a failed call carries", got)
	}
}

// TestEmptyHandleRepliesUnimplemented covers a method the service does not
// expose: the request body is consumed, then the reply names the method.
func TestEmptyHandleRepliesUnimplemented(t *testing.T) {
	fs := &fakeSender{}
	e := &empty{Sender: fs, Codec: encoding.GetCodec(encoding.Name)}
	recv := &fakeRecver{frames: []Buffer{payloadFrame(t, &api.EchoRequest{Message: "hi"})}}

	if err := e.Handle(context.Background(), recv, &CallOptions{StreamID: 8, FullMethodName: "/api.EchoService/Stream"}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if recv.remaining() != 0 {
		t.Error("Handle left the request body on the connection")
	}
	frames := fs.sent()
	header := replyHeader(t, frames)
	if codes.Code(header.Code) != codes.Unimplemented {
		t.Errorf("reply Code = %v, want Unimplemented", codes.Code(header.Code))
	}
	if header.Desc != "/api.EchoService/Stream" {
		t.Errorf("reply Desc = %q, want the requested method name", header.Desc)
	}
	if header.StreamID != 8 {
		t.Errorf("reply StreamID = %d, want 8", header.StreamID)
	}
	if got := len(frames[1]); got != 0 {
		t.Errorf("reply payload = %d bytes, want the zero-length frame an unimplemented call carries", got)
	}
}

// TestClientStreamHandleDropsASecondReply covers a stream whose receive slot is
// already full: the second response cannot be delivered, so Handle hands its
// payload back to the pool and closes the stream instead of parking the receive
// loop. A unary stream carries one reply, so this only fires on a peer that
// answers twice.
func TestClientStreamHandleDropsASecondReply(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	// Occupy the one-slot buffer so the next delivery has nowhere to go.
	s.c <- &reply{Buffer: pooledPayload(nil)}

	recv := &fakeRecver{frames: []Buffer{pooledPayload(nil)}}
	if err := s.Handle(context.Background(), recv); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if s.Context().Err() == nil {
		t.Error("Handle did not close the stream after dropping a second reply")
	}
}

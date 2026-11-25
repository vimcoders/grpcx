package transport

import (
	"context"
	"fmt"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestClientStreamSendWritesHeaderThenPayload pins the two-frame shape: the
// marshalled call options go first so the peer can route the call, then the
// message. Reversing them would leave the receiver waiting for a payload it
// cannot attribute.
func TestClientStreamSendWritesHeaderThenPayload(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.Send(&api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("Send wrote %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.StreamID != s.id {
		t.Fatalf("header StreamID = %d, want %d", header.StreamID, s.id)
	}
	var payload api.EchoRequest
	if err := encoding.Unmarshal(frames[1], &payload); err != nil {
		t.Fatalf("decode payload: %v", err)
	}
	if payload.Message != "hi" {
		t.Fatalf("payload message = %q, want %q", payload.Message, "hi")
	}
}

// TestClientStreamSendForcesItsOwnStreamID pins the routing rule: the stream id
// is applied last, so a caller cannot aim its message at another stream by
// passing an id of its own. The other call options still reach the header.
func TestClientStreamSendForcesItsOwnStreamID(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.Send(&api.EchoRequest{Message: "hi"}, &CallOptions{StreamID: 42, FullMethodName: "/other/Method"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	var header api.CallOptions
	if err := encoding.Unmarshal(fs.sent()[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.StreamID != s.id {
		t.Fatalf("header StreamID = %d, want the stream's own %d", header.StreamID, s.id)
	}
	if header.FullMethodName != "/other/Method" {
		t.Fatalf("header FullMethodName = %q, want the caller's %q", header.FullMethodName, "/other/Method")
	}
}

// TestClientStreamSendReportsMarshalError pins the failure path: an unmarshallable
// message fails the call before any frame is written, so a broken payload cannot
// leave half a request on the wire.
func TestClientStreamSendReportsMarshalError(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.Send(func() {}); err == nil {
		t.Fatal("Send of an unmarshallable message returned nil")
	}
	if frames := fs.sent(); len(frames) != 0 {
		t.Fatalf("%d frames were written for a message that failed to marshal", len(frames))
	}
}

// TestClientStreamRecvMsgUnmarshalsReply pins the success path: the reply payload
// is decoded into the caller's message.
func TestClientStreamRecvMsgUnmarshalsReply(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{
		Options: &api.CallOptions{Code: uint32(codes.OK), Desc: codes.OK.String()},
		Payload: pooledPayload(mustMarshal(t, &api.EchoResponse{Message: "pong"})),
	}

	var reply api.EchoResponse
	if err := s.RecvMsg(&reply); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if reply.Message != "pong" {
		t.Fatalf("reply = %q, want %q", reply.Message, "pong")
	}
}

// TestClientStreamRecvMsgReturnsStatusError pins how a handler failure reaches the
// caller: the code and the description travel in the response header, and the
// payload is not decoded over them.
func TestClientStreamRecvMsgReturnsStatusError(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{
		Options: &api.CallOptions{Code: uint32(codes.InvalidArgument), Desc: "test: handler failed"},
		Payload: pooledPayload([]byte("not a reply")),
	}

	err := s.RecvMsg(&api.EchoResponse{})
	if got := statusCodeOf(err); got != codes.InvalidArgument {
		t.Fatalf("RecvMsg error code = %v, want InvalidArgument", got)
	}
	if !strings.Contains(err.Error(), "test: handler failed") {
		t.Fatalf("RecvMsg error = %v, want it to carry the handler description", err)
	}
}

// TestClientStreamRecvMsgReturnsCanceledWhenStreamEnds pins the end-of-call case:
// a stream closed before its reply arrives reports Canceled instead of blocking
// forever.
func TestClientStreamRecvMsgReturnsCanceledWhenStreamEnds(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := statusCodeOf(s.RecvMsg(&api.EchoResponse{})); got != codes.Canceled {
		t.Fatalf("RecvMsg on a closed stream = %v, want Canceled", got)
	}
}

// TestClientStreamCloseSendDeregistersOnce pins the cleanup: the stream leaves the
// table exactly once no matter how often CloseSend runs, and no frame is written
// because grpcx carries unary calls only.
func TestClientStreamCloseSendDeregistersOnce(t *testing.T) {
	fs := &fakeSender{}
	s, deleted := newStreamUnderTest(fs)
	for range 2 {
		if err := s.CloseSend(); err != nil {
			t.Fatalf("CloseSend: %v", err)
		}
	}
	if got := deleted.Load(); got != 1 {
		t.Fatalf("deregistered %d times, want 1", got)
	}
	if frames := fs.sent(); len(frames) != 0 {
		t.Fatalf("CloseSend wrote %d frames, want none", len(frames))
	}
}

// TestClientStreamCloseCancelContext pins the signal a blocked recv waits on.
func TestClientStreamCloseCancelContext(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-s.Context().Done():
	default:
		t.Fatal("the stream context is still live after Close")
	}
}

// TestClientStreamHandleDeliversReply pins the receive path: the payload off the
// wire lands in the stream's slot together with the header the frame carried, and
// no error is reported.
func TestClientStreamHandleDeliversReply(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	recv := &fakeRecver{frames: [][]byte{pooledPayload(mustMarshal(t, &api.EchoResponse{Message: "pong"}))}}
	header := &api.CallOptions{StreamID: s.id, FullMethodName: "/api.EchoService/Echo", Code: uint32(codes.OK)}

	if err := s.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	reply := <-s.c
	if reply.Options.StreamID != s.id {
		t.Fatalf("reply StreamID = %d, want %d", reply.Options.StreamID, s.id)
	}
	var body api.EchoResponse
	if err := encoding.Unmarshal(reply.Payload, &body); err != nil {
		t.Fatalf("decode delivered payload: %v", err)
	}
	if body.Message != "pong" {
		t.Fatalf("delivered message = %q, want %q", body.Message, "pong")
	}
}

// TestClientStreamHandleReportsRecvError pins that a broken connection ends the
// call instead of delivering an empty reply.
func TestClientStreamHandleReportsRecvError(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	recv := &fakeRecver{err: io.ErrUnexpectedEOF}
	if err := s.Handle(context.Background(), recv, WithCallOptions(&api.CallOptions{StreamID: s.id})); err == nil {
		t.Fatal("Handle on a broken connection returned nil")
	}
}

// TestClientStreamHandleClosesWhenSlotIsTaken pins the drop path: a reply nobody
// is waiting for is returned to the pool and the stream closes, so a frame that
// arrives after the call ended does not hold the buffer or the stream open.
func TestClientStreamHandleClosesWhenSlotIsTaken(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{Options: &api.CallOptions{Code: uint32(codes.OK)}, Payload: []byte("already here")}
	recv := &fakeRecver{frames: [][]byte{pooledPayload([]byte("late"))}}

	if err := s.Handle(context.Background(), recv, WithCallOptions(&api.CallOptions{StreamID: s.id})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	select {
	case <-s.Context().Done():
	default:
		t.Fatal("the stream was left open after its slot could not take the reply")
	}
	if got := len(s.c); got != 1 {
		t.Fatalf("slot holds %d replies, want the original 1", got)
	}
}

// TestClientStreamSendMsgWritesTheStreamFrame pins the grpc.ClientStream shape of
// Send: SendMsg writes the same two frames without any call options, which is what
// a generated stub would drive.
func TestClientStreamSendMsgWritesTheStreamFrame(t *testing.T) {
	fs := &fakeSender{}
	s, _ := newStreamUnderTest(fs)
	if err := s.SendMsg(&api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("SendMsg wrote %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.StreamID != s.id {
		t.Fatalf("header StreamID = %d, want %d", header.StreamID, s.id)
	}
}

// TestClientStreamRecvMsgReportsUnmarshalError pins that a reply the caller's type
// cannot hold is an error, not a silently zero-valued message.
func TestClientStreamRecvMsgReportsUnmarshalError(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{
		Options: &api.CallOptions{Code: uint32(codes.OK), Desc: codes.OK.String()},
		Payload: pooledPayload([]byte{0xff, 0xff, 0xff}),
	}
	if err := s.RecvMsg(&api.EchoResponse{}); err == nil {
		t.Fatal("RecvMsg accepted an undecodable reply")
	}
}

// TestClientStreamHandleClosesWhenCallIsCanceled pins the second drop path: with
// the caller's context already cancelled and the slot taken, the late frame is
// returned to the pool and the stream closes instead of being delivered to a
// call nobody is waiting for.
func TestClientStreamHandleClosesWhenCallIsCanceled(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{Options: &api.CallOptions{Code: uint32(codes.OK)}, Payload: []byte("already here")}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	recv := &fakeRecver{frames: [][]byte{pooledPayload([]byte("late"))}}

	if err := s.Handle(ctx, recv, WithCallOptions(&api.CallOptions{StreamID: s.id})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	select {
	case <-s.Context().Done():
	default:
		t.Fatal("the stream was left open after its call was cancelled")
	}
}

// TestClientStreamHandleClosesWhenStreamEnds pins the third drop path: the stream
// itself was closed while the frame was in flight, so the reply is returned to the
// pool rather than delivered to a call that already ended.
func TestClientStreamHandleClosesWhenStreamEnds(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &api.Response{Options: &api.CallOptions{Code: uint32(codes.OK)}, Payload: []byte("already here")}
	if err := s.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	recv := &fakeRecver{frames: [][]byte{pooledPayload([]byte("late"))}}

	if err := s.Handle(context.Background(), recv, WithCallOptions(&api.CallOptions{StreamID: s.id})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := len(s.c); got != 1 {
		t.Fatalf("slot holds %d replies, want the original 1", got)
	}
}

// TestCallOptionsApplyKeepsTheSmallerTimeout pins the deadline merge: the smaller
// of the two wins, so a short caller deadline is never extended by the
// transport's wider default.
func TestCallOptionsApplyKeepsTheSmallerTimeout(t *testing.T) {
	cases := []struct {
		caller    int64
		transport int64
		want      int64
	}{
		{caller: 50, transport: 100, want: 50},
		{caller: 0, transport: 50, want: 50},
		{caller: 100, transport: 50, want: 50},
	}
	for _, c := range cases {
		out := api.CallOptions{Timeout: c.transport}
		(&CallOptions{Timeout: c.caller}).apply(&out)
		if out.Timeout != c.want {
			t.Errorf("caller %d/transport %d: header timeout = %d, want %d", c.caller, c.transport, out.Timeout, c.want)
		}
	}
}

// TestCallOptionsApplyFillsEmptyHeaderFields pins the precedence: every field the
// caller set replaces an empty header field, and nothing that is already set is
// overwritten.
func TestCallOptionsApplyFillsEmptyHeaderFields(t *testing.T) {
	caller := &CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		Service:        "api.EchoService",
		Length:         12,
		Authority:      "example.com",
		Closed:         true,
		Code:           uint32(codes.OK),
		Desc:           "ok",
		Metadatas:      []string{"k", "v"},
		StreamID:       9,
	}
	out := api.CallOptions{}
	caller.apply(&out)

	if out.FullMethodName != caller.FullMethodName || out.Service != caller.Service {
		t.Fatalf("method/service = %q/%q, want %q/%q", out.FullMethodName, out.Service, caller.FullMethodName, caller.Service)
	}
	if out.StreamID != caller.StreamID {
		t.Fatalf("StreamID = %d, want %d", out.StreamID, caller.StreamID)
	}
	if len(out.Metadatas) != 2 || out.Metadatas[0] != "k" || out.Metadatas[1] != "v" {
		t.Fatalf("Metadatas = %v, want [k v]", out.Metadatas)
	}
}

// TestDecodeCallOptionsReadsTheHeader pins the dispatcher's first step: the header
// frame is decoded into the call options the connection routes by.
func TestDecodeCallOptionsReadsTheHeader(t *testing.T) {
	want := &api.CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		StreamID:       3,
		Metadatas:      []string{"k", "v"},
	}
	recv := &fakeRecver{frames: [][]byte{headerFrame(t, want)}}

	got, err := decodeCallOptions(recv)
	if err != nil {
		t.Fatalf("decodeCallOptions: %v", err)
	}
	if got.FullMethodName != want.FullMethodName || got.StreamID != want.StreamID {
		t.Fatalf("header = %+v, want %+v", got, want)
	}
}

// TestDecodeCallOptionsReportsAFailedRead pins that a broken connection is
// reported to the caller rather than decoded into an empty header, which would
// dispatch a call to a stream nobody opened.
func TestDecodeCallOptionsReportsAFailedRead(t *testing.T) {
	recv := &fakeRecver{err: io.ErrUnexpectedEOF}
	if _, err := decodeCallOptions(recv); err == nil {
		t.Fatal("decodeCallOptions on a broken connection returned nil")
	}
}

// TestDecodeCallOptionsReportsABadFrame pins the torn-header case: bytes that do
// not decode are an error, not an empty header.
func TestDecodeCallOptionsReportsABadFrame(t *testing.T) {
	recv := &fakeRecver{frames: [][]byte{pooledPayload([]byte{0xff, 0xff, 0xff})}}
	if _, err := decodeCallOptions(recv); err == nil {
		t.Fatal("decodeCallOptions on an undecodable frame returned nil")
	}
}

// TestWithCallOptionsReplaysTheHeader pins the hand-over: the handler sees the
// call it was dispatched from, field for field, including the metadata pair the
// header carried.
func TestWithCallOptionsReplaysTheHeader(t *testing.T) {
	header := &api.CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		Service:        "api.EchoService",
		Length:         7,
		Authority:      "example.com",
		Timeout:        50,
		Metadatas:      []string{"k", "v"},
		Code:           uint32(codes.OK),
		Desc:           "ok",
		StreamID:       11,
	}
	out := api.CallOptions{}
	WithCallOptions(header).apply(&out)

	if out.FullMethodName != header.FullMethodName || out.Service != header.Service || out.StreamID != header.StreamID {
		t.Fatalf("replayed header = %q/%q/%d, want %q/%q/%d", out.FullMethodName, out.Service, out.StreamID, header.FullMethodName, header.Service, header.StreamID)
	}
	if out.Timeout != header.Timeout || len(out.Metadatas) != len(header.Metadatas) {
		t.Fatalf("replayed timeout/metadata = %d/%v, want %d/%v", out.Timeout, out.Metadatas, header.Timeout, header.Metadatas)
	}
}

// TestMethodHandlerRunsHandlerAndReplies pins the server-side success path: the
// request payload is decoded through the generated handler, and the reply goes
// back on the stream the request came from with an OK header.
func TestMethodHandlerRunsHandlerAndReplies(t *testing.T) {
	fs := &fakeSender{}
	recv := &fakeRecver{frames: [][]byte{pooledPayload(mustMarshal(t, &api.EchoRequest{Message: "hi"}))}}
	opts := DefaultServerOptions
	d := &defaultMethodHandler{
		Sender:   fs,
		Options:  opts.Options,
		imp:      &echoImpl{},
		unaryInt: opts.unaryInt,
		Handler:  api.EchoService_ServiceDesc.Methods[0].Handler,
	}

	header := &api.CallOptions{StreamID: 5, FullMethodName: api.EchoService_Echo_FullMethodName}
	if err := d.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("reply wrote %d frames, want 2", len(frames))
	}
	var replyHeader api.CallOptions
	if err := encoding.Unmarshal(frames[0], &replyHeader); err != nil {
		t.Fatalf("decode reply header: %v", err)
	}
	if replyHeader.StreamID != header.StreamID {
		t.Fatalf("reply StreamID = %d, want %d", replyHeader.StreamID, header.StreamID)
	}
	if got := codes.Code(replyHeader.Code); got != codes.OK {
		t.Fatalf("reply code = %v, want OK", got)
	}
	var body api.EchoResponse
	if err := encoding.Unmarshal(frames[1], &body); err != nil {
		t.Fatalf("decode reply payload: %v", err)
	}
	if body.Message != "hi!" {
		t.Fatalf("reply = %q, want %q", body.Message, "hi!")
	}
}

// TestMethodHandlerReportsHandlerErrorAsStatus pins the error contract: a handler
// failure travels in the reply header's code and description, so the connection
// stays usable for the next call instead of being torn down.
func TestMethodHandlerReportsHandlerErrorAsStatus(t *testing.T) {
	fs := &fakeSender{}
	recv := &fakeRecver{frames: [][]byte{pooledPayload(mustMarshal(t, &api.EchoRequest{Message: "boom"}))}}
	opts := DefaultServerOptions
	d := &defaultMethodHandler{
		Sender:   fs,
		Options:  opts.Options,
		imp:      &echoImpl{failOn: "boom"},
		unaryInt: opts.unaryInt,
		Handler:  api.EchoService_ServiceDesc.Methods[0].Handler,
	}

	header := &api.CallOptions{StreamID: 5, FullMethodName: api.EchoService_Echo_FullMethodName}
	if err := d.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle reported the handler error instead of sending it: %v", err)
	}

	var replyHeader api.CallOptions
	if err := encoding.Unmarshal(fs.sent()[0], &replyHeader); err != nil {
		t.Fatalf("decode reply header: %v", err)
	}
	if got := codes.Code(replyHeader.Code); got != codes.InvalidArgument {
		t.Fatalf("reply code = %v, want InvalidArgument", got)
	}
	// The description is the handler error's own text, so the client can show it:
	// status.Error wraps the code into the message, and the whole error is what
	// travels back.
	if !strings.Contains(replyHeader.Desc, "test: handler failed") {
		t.Fatalf("reply description = %q, want it to carry the handler's message", replyHeader.Desc)
	}
}

// TestMethodHandlerArmsTheCallDeadline pins that the timeout the client advertised
// reaches the handler as a context deadline, so a slow handler is bounded by the
// caller's budget rather than the transport's default.
func TestMethodHandlerArmsTheCallDeadline(t *testing.T) {
	fs := &fakeSender{}
	recv := &fakeRecver{frames: [][]byte{pooledPayload(mustMarshal(t, &api.EchoRequest{Message: "probe"}))}}
	opts := DefaultServerOptions
	d := &defaultMethodHandler{
		Sender:  fs,
		Options: opts.Options,
		imp:     &echoImpl{},
		Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(&api.EchoRequest{}); err != nil {
				return nil, err
			}
			deadline, ok := ctx.Deadline()
			if !ok {
				return nil, status.Error(codes.Internal, "no deadline")
			}
			if left := time.Until(deadline); left <= 0 || left > 50*time.Millisecond {
				return nil, status.Error(codes.Internal, fmt.Sprintf("deadline is %v away", left))
			}
			return &api.EchoResponse{Message: "ok"}, nil
		},
	}

	header := &api.CallOptions{StreamID: 5, Timeout: 50}
	if err := d.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var replyHeader api.CallOptions
	if err := encoding.Unmarshal(fs.sent()[0], &replyHeader); err != nil {
		t.Fatalf("decode reply header: %v", err)
	}
	if got := codes.Code(replyHeader.Code); got != codes.OK {
		t.Fatalf("reply code = %v (%s), want OK", got, replyHeader.Desc)
	}
}

// TestMethodHandlerCarriesMetadata pins that the metadata pairs in the request
// header reach the handler through its context, which is how a service reads
// trace ids and auth.
func TestMethodHandlerCarriesMetadata(t *testing.T) {
	fs := &fakeSender{}
	recv := &fakeRecver{frames: [][]byte{pooledPayload(mustMarshal(t, &api.EchoRequest{Message: "probe"}))}}
	opts := DefaultServerOptions
	d := &defaultMethodHandler{
		Sender:  fs,
		Options: opts.Options,
		imp:     &echoImpl{},
		Handler: func(_ any, ctx context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			if err := dec(&api.EchoRequest{}); err != nil {
				return nil, err
			}
			v, ok := metadata.GetMetadataValue(ctx, "k")
			if !ok || v != "v" {
				return nil, status.Error(codes.Internal, fmt.Sprintf("metadata k = %q/%v", v, ok))
			}
			return &api.EchoResponse{Message: "ok"}, nil
		},
	}

	header := &api.CallOptions{StreamID: 5, Metadatas: []string{"k", "v"}}
	if err := d.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	var replyHeader api.CallOptions
	if err := encoding.Unmarshal(fs.sent()[0], &replyHeader); err != nil {
		t.Fatalf("decode reply header: %v", err)
	}
	if got := codes.Code(replyHeader.Code); got != codes.OK {
		t.Fatalf("reply code = %v (%s), want OK", got, replyHeader.Desc)
	}
}

// TestMethodHandlerReportsRecvError pins that a broken request read ends the call:
// there is nothing to answer with, so the error goes back to the connection loop
// which then closes it.
func TestMethodHandlerReportsRecvError(t *testing.T) {
	d := &defaultMethodHandler{
		Sender:  &fakeSender{},
		Options: DefaultServerOptions.Options,
		Handler: api.EchoService_ServiceDesc.Methods[0].Handler,
	}
	recv := &fakeRecver{err: io.ErrUnexpectedEOF}
	if err := d.Handle(context.Background(), recv, WithCallOptions(&api.CallOptions{StreamID: 5})); err == nil {
		t.Fatal("Handle on a broken request read returned nil")
	}
}

// TestEmptyHandlerReportsRecvError pins the fallback's own failure path: with no
// request payload there is nothing to answer, so the error goes back to the
// connection loop.
func TestEmptyHandlerReportsRecvError(t *testing.T) {
	e := &empty{Codec: encoding.GetCodec(encoding.Name), Sender: &fakeSender{}}
	if err := e.Handle(context.Background(), &fakeRecver{err: io.ErrUnexpectedEOF}, WithCallOptions(&api.CallOptions{StreamID: 5})); err == nil {
		t.Fatal("the fallback returned nil on a broken request read")
	}
}

// TestEmptyHandlerAnswersUnimplemented pins the fallback: an unknown method is
// answered with Unimplemented carrying the requested name, after its payload is
// consumed so the next frame stays on a boundary.
func TestEmptyHandlerAnswersUnimplemented(t *testing.T) {
	fs := &fakeSender{}
	recv := &fakeRecver{frames: [][]byte{pooledPayload([]byte("unknown method payload"))}}
	e := &empty{Codec: encoding.GetCodec(encoding.Name), Sender: fs}
	header := &api.CallOptions{StreamID: 9, FullMethodName: "/api.Nope/Method"}

	if err := e.Handle(context.Background(), recv, WithCallOptions(header)); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	frames := fs.sent()
	if len(frames) != 2 {
		t.Fatalf("fallback wrote %d frames, want 2", len(frames))
	}
	var replyHeader api.CallOptions
	if err := encoding.Unmarshal(frames[0], &replyHeader); err != nil {
		t.Fatalf("decode reply header: %v", err)
	}
	if got := codes.Code(replyHeader.Code); got != codes.Unimplemented {
		t.Fatalf("reply code = %v, want Unimplemented", got)
	}
	if replyHeader.Desc != header.FullMethodName {
		t.Fatalf("reply description = %q, want the requested method %q", replyHeader.Desc, header.FullMethodName)
	}
	if replyHeader.StreamID != header.StreamID {
		t.Fatalf("reply StreamID = %d, want %d", replyHeader.StreamID, header.StreamID)
	}
}

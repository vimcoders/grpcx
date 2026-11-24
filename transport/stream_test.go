package transport

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/codes"
)

// echoServer is the service implementation the stream tests dispatch to. It
// echoes the request with a suffix, so a test can tell the reply apart from the
// request that produced it.
type echoServer struct {
	api.UnimplementedEchoServiceServer
}

func (s *echoServer) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.GetMessage() + "!"}, nil
}

// failingEcho returns a status error, which the handler has to report through
// the response header instead of tearing the connection down.
type failingEcho struct {
	api.UnimplementedEchoServiceServer
}

func (s *failingEcho) Echo(context.Context, *api.EchoRequest) (*api.EchoResponse, error) {
	return nil, status.Error(codes.InvalidArgument, "bad input")
}

// TestCallOptionsApplyMergesFields pins the merge table: a non-zero field in the
// caller's option wins, and a timeout is only ever narrowed. Extending a caller
// deadline here would silently turn a bounded call into an unbounded one.
func TestCallOptionsApplyMergesFields(t *testing.T) {
	base := &api.CallOptions{
		Timeout:        100,
		FullMethodName: "/api.EchoService/Old",
		Service:        "old",
		Metadatas:      []string{"a", "1"},
		StreamID:       7,
	}
	overlay := CallOptions{
		Timeout:        40,
		FullMethodName: "/api.EchoService/Echo",
		Metadatas:      []string{"b", "2"},
	}

	overlay.apply(base)

	if base.Timeout != 40 {
		t.Errorf("Timeout = %d, want the smaller 40", base.Timeout)
	}
	if base.FullMethodName != "/api.EchoService/Echo" {
		t.Errorf("FullMethodName = %q, want the overlay value", base.FullMethodName)
	}
	if base.Service != "old" {
		t.Errorf("Service = %q, want the zero overlay to keep the base value", base.Service)
	}
	if base.StreamID != 7 {
		t.Errorf("StreamID = %d, want the zero overlay to keep the base value", base.StreamID)
	}
	if got, want := len(base.Metadatas), 4; got != want {
		t.Errorf("len(Metadatas) = %d, want %d (overlay appended)", got, want)
	}
}

// TestCallOptionsApplyKeepsShorterTimeout pins the other half of the timeout
// rule: an overlay demanding a longer deadline than the header already carries
// must not extend it.
func TestCallOptionsApplyKeepsShorterTimeout(t *testing.T) {
	base := &api.CallOptions{Timeout: 20}
	overlay := CallOptions{Timeout: 90}

	overlay.apply(base)

	if base.Timeout != 20 {
		t.Errorf("Timeout = %d, want the shorter 20 preserved", base.Timeout)
	}
}

// TestCallOptionsApplyZeroTimeoutLeavesBase pins that an option carrying no
// timeout is a no-op, so a caller without a deadline never rewrites the header's
// advertised default.
func TestCallOptionsApplyZeroTimeoutLeavesBase(t *testing.T) {
	base := &api.CallOptions{Timeout: 300}
	overlay := CallOptions{FullMethodName: "/api.EchoService/Echo"}

	overlay.apply(base)

	if base.Timeout != 300 {
		t.Errorf("Timeout = %d, want the untouched 300", base.Timeout)
	}
}

// TestWithCallOptionsCopiesHeader pins that the dispatcher hands the handler a
// copy of the decoded header rather than the header itself: the handler applies
// its own defaults through CallOptions.apply, which would mutate the frame
// options the dispatcher still holds.
func TestWithCallOptionsCopiesHeader(t *testing.T) {
	header := &api.CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		StreamID:       11,
		Timeout:        500,
		Metadatas:      []string{"k", "v"},
	}

	opt := WithCallOptions(header)
	var got api.CallOptions
	opt.apply(&got)

	if got.StreamID != 11 || got.FullMethodName != "/api.EchoService/Echo" || got.Timeout != 500 {
		t.Fatalf("applied header = %+v, want the decoded values", &got)
	}
	if len(got.Metadatas) != 2 {
		t.Fatalf("Metadatas = %v, want the decoded pair", got.Metadatas)
	}
}

// TestSenderWritesHeaderThenPayload pins the frame order a peer depends on:
// header first, payload second, never interleaved, so the reader can tell the
// two apart by position alone.
func TestSenderWritesHeaderThenPayload(t *testing.T) {
	rs := &recordSender{}
	sd := &sender{Sender: rs, Codec: encoding.GetCodec(encoding.Name)}

	err := sd.send(9, &api.EchoRequest{Message: "hi"}, &CallOptions{FullMethodName: "/api.EchoService/Echo"})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := rs.count(); got != 1 {
		t.Fatalf("Send calls = %d, want 1", got)
	}
	if got := len(rs.calls[0]); got != 2 {
		t.Fatalf("frames = %d, want header plus payload", got)
	}

	header := decodeHeader(t, rs.frame(0, 0))
	if header.FullMethodName != "/api.EchoService/Echo" {
		t.Errorf("header FullMethodName = %q", header.FullMethodName)
	}
	if header.StreamID != 9 {
		t.Errorf("header StreamID = %d, want 9", header.StreamID)
	}

	var req api.EchoRequest
	if err := encoding.Unmarshal(rs.frame(0, 1), &req); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if req.Message != "hi" {
		t.Errorf("payload message = %q, want %q", req.Message, "hi")
	}
}

// TestSenderStreamIDWinsOverOption pins that the id argument overrides whatever
// id a call option carries, so a caller cannot route a frame onto another
// stream by filling in the field itself.
func TestSenderStreamIDWinsOverOption(t *testing.T) {
	rs := &recordSender{}
	sd := &sender{Sender: rs, Codec: encoding.GetCodec(encoding.Name)}

	if err := sd.send(42, &api.Empty{}, &CallOptions{StreamID: 1}); err != nil {
		t.Fatalf("send: %v", err)
	}
	if got := decodeHeader(t, rs.frame(0, 0)).StreamID; got != 42 {
		t.Fatalf("header StreamID = %d, want the send argument 42", got)
	}
}

// TestSenderReportsMarshalError pins that a message the codec cannot marshal is
// reported before anything reaches the wire, so the peer never sees a header
// without its payload.
func TestSenderReportsMarshalError(t *testing.T) {
	rs := &recordSender{}
	sd := &sender{Sender: rs, Codec: encoding.GetCodec(encoding.Name)}

	if err := sd.send(1, make(chan int)); err == nil {
		t.Fatal("send of an unmarshalable message = nil error")
	}
	if rs.count() != 0 {
		t.Fatalf("Send calls = %d, want none after a marshal failure", rs.count())
	}
}

// TestSenderReportsSendError pins that a transport failure is surfaced to the
// caller: the frame writer is the only place that knows the socket refused the
// write, and returning nil would report a call that never left.
func TestSenderReportsSendError(t *testing.T) {
	rs := &recordSender{}
	rs.fail(errWriteRefused)
	sd := &sender{Sender: rs, Codec: encoding.GetCodec(encoding.Name)}

	if err := sd.send(1, &api.EchoRequest{Message: "x"}); !errors.Is(err, errWriteRefused) {
		t.Fatalf("send = %v, want the transport's error", err)
	}
}

// TestDecodeCallOptionsReadsHeader pins that the dispatcher consumes exactly the
// header frame and returns what the wire carried, since both directions use it
// to find the stream before any payload is read.
func TestDecodeCallOptionsReadsHeader(t *testing.T) {
	src := &frameSource{frames: [][]byte{headerFrame(t, &api.CallOptions{StreamID: 5, FullMethodName: "/m"})}}

	opts, err := decodeCallOptions(src)
	if err != nil {
		t.Fatalf("decodeCallOptions: %v", err)
	}
	if opts.StreamID != 5 || opts.FullMethodName != "/m" {
		t.Fatalf("decoded = %+v, want the frame's values", opts)
	}
	if src.i != 1 {
		t.Fatalf("consumed %d frames, want 1", src.i)
	}
}

// TestDecodeCallOptionsReportsBrokenHeader pins that a garbage header is an
// error rather than an empty options struct: silently defaulting it would route
// the call to nothing and leave the connection on a frame boundary it did not
// actually reach.
func TestDecodeCallOptionsReportsBrokenHeader(t *testing.T) {
	src := &frameSource{frames: [][]byte{newFrame([]byte{0xff, 0xff})}}

	if _, err := decodeCallOptions(src); err == nil {
		t.Fatal("decodeCallOptions = nil error, want a marshal failure")
	}
}

// TestDecodeCallOptionsReportsRecvError pins that a connection ending before a
// header arrives is passed through as-is, so the caller can tell EOF from a bad
// frame.
func TestDecodeCallOptionsReportsRecvError(t *testing.T) {
	src := &frameSource{}

	if _, err := decodeCallOptions(src); !errors.Is(err, io.EOF) {
		t.Fatalf("decodeCallOptions = %v, want io.EOF", err)
	}
}

// newTestStream builds a client stream wired to a record sender, with the pieces
// a real stream gets from createStream. It returns the stream and the recorded
// deletes so a test can assert cleanup.
func newTestStream(t *testing.T) (*clientStream, *recordSender, *int) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	rs := &recordSender{}
	deletes := 0
	s := &clientStream{
		Sender:       rs,
		ctx:          ctx,
		id:           7,
		c:            make(chan *api.Response, 1),
		cancel:       cancel,
		deleteStream: func(uint32) { deletes++ },
		codec:        encoding.GetCodec(encoding.Name),
	}
	return s, rs, &deletes
}

// TestClientStreamSendUsesItsOwnID pins that a stream routes its own sends: the
// frame carries the id the stream was created with, regardless of what the call
// option says.
func TestClientStreamSendUsesItsOwnID(t *testing.T) {
	s, rs, _ := newTestStream(t)

	if err := s.Send(&api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if got := decodeHeader(t, rs.frame(0, 0)).StreamID; got != s.id {
		t.Fatalf("header StreamID = %d, want %d", got, s.id)
	}
}

// TestClientStreamSendMsgUsesItsOwnID pins the grpc.ClientStream shape of the
// same rule.
func TestClientStreamSendMsgUsesItsOwnID(t *testing.T) {
	s, rs, _ := newTestStream(t)

	if err := s.SendMsg(&api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	if got := decodeHeader(t, rs.frame(0, 0)).StreamID; got != s.id {
		t.Fatalf("header StreamID = %d, want %d", got, s.id)
	}
}

// TestClientStreamRecvMsgDecodesReply pins the success path: a reply framed on
// the stream is unmarshalled into the caller's message.
func TestClientStreamRecvMsgDecodesReply(t *testing.T) {
	s, _, _ := newTestStream(t)
	s.c <- &api.Response{
		Options: &api.CallOptions{Code: uint32(codes.OK)},
		Payload: newFrame(mustMarshal(t, &api.EchoResponse{Message: "pong"})),
	}

	var out api.EchoResponse
	if err := s.RecvMsg(&out); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if out.Message != "pong" {
		t.Fatalf("reply = %q, want pong", out.Message)
	}
}

// TestClientStreamRecvMsgReportsStatus pins that a non-OK reply code becomes a
// status error carrying that code, which is how the server reports handler
// failures without breaking the connection.
func TestClientStreamRecvMsgReportsStatus(t *testing.T) {
	s, _, _ := newTestStream(t)
	s.c <- &api.Response{
		Options: &api.CallOptions{Code: uint32(codes.InvalidArgument), Desc: "bad input"},
		Payload: newFrame(mustMarshal(t, &api.EchoResponse{})),
	}

	err := s.RecvMsg(&api.EchoResponse{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("RecvMsg code = %v, want InvalidArgument", status.Code(err))
	}
	// status.Error renders the description inside the "rpc error: ..." wrapper,
	// so the message part is what carries the server's text.
	if got := status.Convert(err).Message(); got != "bad input" {
		t.Fatalf("RecvMsg message = %q, want the server description", got)
	}
}

// TestClientStreamRecvMsgReportsCanceled pins that a stream canceled before its
// reply arrives ends the call with Canceled instead of blocking forever.
func TestClientStreamRecvMsgReportsCanceled(t *testing.T) {
	s, _, _ := newTestStream(t)
	s.cancel()

	if err := s.RecvMsg(&api.EchoResponse{}); status.Code(err) != codes.Canceled {
		t.Fatalf("RecvMsg after cancel = %v, want Canceled", err)
	}
}

// TestClientStreamCloseSendDeletesOnce pins the exactly-once deregistration:
// CloseSend is called twice on every call — once by the caller's defer, once by
// its own cleanup — and a second delete would drop a stream that a later frame
// still routes to.
func TestClientStreamCloseSendDeletesOnce(t *testing.T) {
	s, _, deletes := newTestStream(t)

	if err := s.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("second CloseSend: %v", err)
	}
	if *deletes != 1 {
		t.Fatalf("deleted %d times, want 1", *deletes)
	}
}

// TestClientStreamHandleDeliversResponse pins the receive path: the payload
// frame off the wire lands in the stream's slot with the header that prefixed
// it.
func TestClientStreamHandleDeliversResponse(t *testing.T) {
	s, _, _ := newTestStream(t)
	want := "pong"
	src := &frameSource{frames: [][]byte{newFrame(mustMarshal(t, &api.EchoResponse{Message: want}))}}

	err := s.Handle(context.Background(), src, WithCallOptions(&api.CallOptions{StreamID: s.id}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	var got api.EchoResponse
	if err := s.RecvMsg(&got); err != nil {
		t.Fatalf("RecvMsg: %v", err)
	}
	if got.Message != want {
		t.Fatalf("reply = %q, want %q", got.Message, want)
	}
}

// TestClientStreamHandleReportsRecvError pins that a broken connection below the
// frame is returned to the receive loop, which is the only place that can log it
// and drop the connection.
func TestClientStreamHandleReportsRecvError(t *testing.T) {
	s, _, _ := newTestStream(t)
	src := &frameSource{err: io.ErrUnexpectedEOF}

	if err := s.Handle(context.Background(), src); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Handle = %v, want ErrUnexpectedEOF", err)
	}
}

// TestClientStreamHandleDropsWhenSlotIsFull pins the abandoned-call path: once
// the reply slot is full and the call is canceled, the frame is consumed off the
// wire and its buffer returned, so the next frame still starts on a boundary.
func TestClientStreamHandleDropsWhenSlotIsFull(t *testing.T) {
	s, _, _ := newTestStream(t)
	s.c <- &api.Response{Options: &api.CallOptions{}} // occupy the single slot

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	src := &frameSource{frames: [][]byte{newFrame(mustMarshal(t, &api.EchoResponse{Message: "dropped"}))}}

	if err := s.Handle(ctx, src); err != nil {
		t.Fatalf("Handle on a canceled call: %v", err)
	}
	if src.i != 1 {
		t.Fatalf("consumed %d frames, want the dropped frame read off the wire", src.i)
	}
	select {
	case <-s.Context().Done():
	default:
		t.Fatal("stream still live after dropping an undeliverable frame, want it closed")
	}
}

// TestDefaultMethodHandlerReplies pins the server-side success path: the
// dispatch table resolves the method, the handler runs, and the reply goes back
// on the stream the request arrived on with code OK.
func TestDefaultMethodHandlerReplies(t *testing.T) {
	rs := &recordSender{}
	h := &defaultMethodHandler{
		Sender:   rs,
		Options:  DefaultOptions(),
		imp:      &echoServer{},
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  api.EchoService_ServiceDesc.Methods[0].Handler,
	}
	src := &frameSource{frames: [][]byte{newFrame(mustMarshal(t, &api.EchoRequest{Message: "ping"}))}}

	err := h.Handle(context.Background(), src, WithCallOptions(&api.CallOptions{StreamID: 12}))
	if err != nil {
		t.Fatalf("Handle: %v", err)
	}

	header := decodeHeader(t, rs.frame(0, 0))
	if header.StreamID != 12 {
		t.Errorf("reply StreamID = %d, want the request's 12", header.StreamID)
	}
	if codes.Code(header.Code) != codes.OK {
		t.Errorf("reply code = %v, want OK", codes.Code(header.Code))
	}

	var reply api.EchoResponse
	if err := encoding.Unmarshal(rs.frame(0, 1), &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "ping!" {
		t.Errorf("reply = %q, want ping!", reply.Message)
	}
}

// TestDefaultMethodHandlerReportsHandlerError pins that a handler error becomes
// the reply's status code instead of a connection teardown, so one failed call
// does not take the connection away from the others multiplexed on it.
func TestDefaultMethodHandlerReportsHandlerError(t *testing.T) {
	rs := &recordSender{}
	h := &defaultMethodHandler{
		Sender:   rs,
		Options:  DefaultOptions(),
		imp:      &failingEcho{},
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  api.EchoService_ServiceDesc.Methods[0].Handler,
	}
	src := &frameSource{frames: [][]byte{newFrame(mustMarshal(t, &api.EchoRequest{Message: "ping"}))}}

	if err := h.Handle(context.Background(), src, WithCallOptions(&api.CallOptions{StreamID: 3})); err != nil {
		t.Fatalf("Handle: %v", err)
	}

	header := decodeHeader(t, rs.frame(0, 0))
	if codes.Code(header.Code) != codes.InvalidArgument {
		t.Fatalf("reply code = %v, want InvalidArgument", codes.Code(header.Code))
	}
	// The handler records err.Error(), not the bare status message, so the
	// description carries the whole "rpc error: ..." string.
	if !strings.Contains(header.Desc, "bad input") {
		t.Fatalf("reply desc = %q, want the handler's error text", header.Desc)
	}
	if header.StreamID != 3 {
		t.Fatalf("reply StreamID = %d, want 3", header.StreamID)
	}
}

// TestEmptyHandlerAnswersUnimplemented pins the fallback for an unknown or
// unsupported method: the payload is still consumed so the connection stays
// frame-aligned, and the request's method name comes back as the description.
func TestEmptyHandlerAnswersUnimplemented(t *testing.T) {
	rs := &recordSender{}
	e := &empty{Sender: rs, Codec: encoding.GetCodec(encoding.Name)}
	src := &frameSource{frames: [][]byte{newFrame(mustMarshal(t, &api.EchoRequest{Message: "ignored"}))}}

	if err := e.Handle(context.Background(), src, WithCallOptions(&api.CallOptions{StreamID: 4, FullMethodName: "/nope/Nope"})); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if src.i != 1 {
		t.Fatalf("consumed %d frames, want the request payload read off the wire", src.i)
	}

	header := decodeHeader(t, rs.frame(0, 0))
	if codes.Code(header.Code) != codes.Unimplemented {
		t.Fatalf("reply code = %v, want Unimplemented", codes.Code(header.Code))
	}
	if header.Desc != "/nope/Nope" {
		t.Fatalf("reply desc = %q, want the requested method", header.Desc)
	}
	if header.StreamID != 4 {
		t.Fatalf("reply StreamID = %d, want 4", header.StreamID)
	}
}

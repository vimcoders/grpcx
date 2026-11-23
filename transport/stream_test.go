package transport

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestCallOptionsApplyMerge pins the merge rules: a non-zero caller field wins,
// metadata is appended, and a timeout never grows when the caller's is shorter.
func TestCallOptionsApplyMerge(t *testing.T) {
	base := api.CallOptions{
		Service:        "api.EchoService",
		FullMethodName: "/api.EchoService/Echo",
		Timeout:        100,
		Metadatas:      []string{"a", "1"},
		StreamID:       3,
		Authority:      "peer",
	}
	caller := CallOptions{
		FullMethodName: "/other/Method",
		Timeout:        50,
		Metadatas:      []string{"b", "2"},
		StreamID:       9,
		Authority:      "caller",
	}
	caller.apply(&base)

	if base.FullMethodName != "/other/Method" {
		t.Fatalf("FullMethodName = %q, want the caller's", base.FullMethodName)
	}
	if base.Service != "api.EchoService" {
		t.Fatalf("Service = %q, want the base value kept", base.Service)
	}
	if base.Timeout != 50 {
		t.Fatalf("Timeout = %d, want the smaller caller value 50", base.Timeout)
	}
	if base.StreamID != 9 {
		t.Fatalf("StreamID = %d, want the caller's 9", base.StreamID)
	}
	if got := len(base.Metadatas); got != 4 {
		t.Fatalf("Metadatas length = %d, want 4 (appended)", got)
	}
}

// TestCallOptionsApplyKeepsShorterTimeout is the other half of the timeout
// rule: a longer caller deadline must not extend the already negotiated one.
func TestCallOptionsApplyKeepsShorterTimeout(t *testing.T) {
	base := api.CallOptions{Timeout: 50}
	caller := CallOptions{Timeout: 500}
	caller.apply(&base)
	if base.Timeout != 50 {
		t.Fatalf("Timeout = %d, want 50", base.Timeout)
	}
}

// TestCallOptionsApplyZeroLeavesBaseAlone checks that an empty CallOptions is a
// no-op, which is what an omitted option must mean.
func TestCallOptionsApplyZeroLeavesBaseAlone(t *testing.T) {
	base := api.CallOptions{FullMethodName: "/m", Timeout: 20, StreamID: 2}
	empty := CallOptions{}
	empty.apply(&base)
	if base.FullMethodName != "/m" || base.Timeout != 20 || base.StreamID != 2 {
		t.Fatalf("base mutated by an empty option: method=%q timeout=%d stream=%d",
			base.FullMethodName, base.Timeout, base.StreamID)
	}
}

// TestWithCallOptionsCopiesFields makes sure the dispatcher's decoded header
// reaches the handler intact.
func TestWithCallOptionsCopiesFields(t *testing.T) {
	read := &api.CallOptions{
		Service:        "svc",
		FullMethodName: "/svc/M",
		Timeout:        42,
		Metadatas:      []string{"k", "v"},
		Code:           3,
		Desc:           "desc",
		StreamID:       11,
	}
	var got api.CallOptions
	WithCallOptions(read).apply(&got)

	if got.FullMethodName != read.FullMethodName || got.Timeout != read.Timeout ||
		got.Code != read.Code || got.StreamID != read.StreamID ||
		got.StreamID != 11 || len(got.Metadatas) != 2 {
		t.Fatalf("copied options = method=%q timeout=%d code=%d stream=%d, want the ones from the wire",
			got.FullMethodName, got.Timeout, got.Code, got.StreamID)
	}
}

// TestSenderWritesHeaderBeforePayload is the frame contract every peer depends
// on: the marshalled options travel first, the marshalled message second.
func TestSenderWritesHeaderBeforePayload(t *testing.T) {
	sink := &frameSink{}
	s := sender{Codec: encoding.GetCodec(encoding.Name), Sender: sink}
	if err := s.send(4, &api.EchoRequest{Message: "hi"}, &CallOptions{FullMethodName: "/m"}); err != nil {
		t.Fatalf("send: %v", err)
	}

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}

	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.FullMethodName != "/m" || header.StreamID != 4 {
		t.Fatalf("header = method=%q stream=%d, want method /m on stream 4", header.FullMethodName, header.StreamID)
	}

	var payload api.EchoRequest
	if err := encoding.Unmarshal(frames[1], &payload); err != nil {
		t.Fatalf("unmarshal payload: %v", err)
	}
	if payload.Message != "hi" {
		t.Fatalf("payload = %q, want %q", payload.Message, "hi")
	}
}

// TestSenderStreamIDWinsOverOptions locks the routing invariant of the
// explicit stream id parameter: an id smuggled in through options must not
// move the frame onto another stream.
func TestSenderStreamIDWinsOverOptions(t *testing.T) {
	sink := &frameSink{}
	s := sender{Codec: encoding.GetCodec(encoding.Name), Sender: sink}
	if err := s.send(5, &api.EchoRequest{Message: "hi"}, &CallOptions{StreamID: 42}); err != nil {
		t.Fatalf("send: %v", err)
	}

	var header api.CallOptions
	if err := encoding.Unmarshal(sink.Frames()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.StreamID != 5 {
		t.Fatalf("StreamID = %d, want the explicit 5", header.StreamID)
	}
}

// TestClientStreamSendAppendsOwnStreamID is the property that keeps a caller
// from steering a frame onto somebody else's stream: the stream's own id is
// applied last and wins.
func TestClientStreamSendAppendsOwnStreamID(t *testing.T) {
	sink := &frameSink{}
	s := &clientStream{id: 7, Sender: sink, codec: encoding.GetCodec(encoding.Name)}

	if err := s.Send(&api.EchoRequest{Message: "x"}, &CallOptions{StreamID: 99}); err != nil {
		t.Fatalf("send: %v", err)
	}

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.StreamID != 7 {
		t.Fatalf("StreamID = %d, want the stream's own 7", header.StreamID)
	}
}

// TestClientStreamSendMsgAndContext covers the grpc.ClientStream accessors a
// stub or an interceptor may reach for: SendMsg sends on the stream's own id
// and Context hands back the stream context, not the caller's.
func TestClientStreamSendMsgAndContext(t *testing.T) {
	sink := &frameSink{}
	ctx := t.Context()
	s := &clientStream{
		Sender: sink,
		ctx:    ctx,
		id:     9,
		codec:  encoding.GetCodec(encoding.Name),
	}

	if err := s.SendMsg(&api.EchoRequest{Message: "msg"}); err != nil {
		t.Fatalf("SendMsg: %v", err)
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(sink.Frames()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.StreamID != 9 {
		t.Fatalf("StreamID = %d, want 9", header.StreamID)
	}
	if s.Context() != ctx {
		t.Fatal("Context() did not return the stream context")
	}
}

// TestDecodeCallOptionsRoundTrip reads back a header the sender wrote.
func TestDecodeCallOptionsRoundTrip(t *testing.T) {
	h, err := encoding.Marshal(&api.CallOptions{FullMethodName: "/m", StreamID: 12})
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}

	got, err := decodeCallOptions(newFrameSource(h))
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.FullMethodName != "/m" || got.StreamID != 12 {
		t.Fatalf("decoded = %+v, want /m on stream 12", got)
	}
}

// TestDecodeCallOptionsRejectsGarbage makes a malformed header an error rather
// than a silently zeroed dispatch target.
func TestDecodeCallOptionsRejectsGarbage(t *testing.T) {
	if _, err := decodeCallOptions(newFrameSource([]byte{0xff, 0xff, 0xff})); err == nil {
		t.Fatal("decode succeeded on garbage, want an error")
	}
}

// TestDefaultMethodHandlerReplies pins the success path: the handler's reply is
// marshalled into the payload frame and the header carries OK on the same
// stream the request arrived on.
func TestDefaultMethodHandlerReplies(t *testing.T) {
	impl := &testEcho{}
	sink := &frameSink{}
	h := &defaultMethodHandler{
		Sender:   sink,
		Options:  DefaultOptions(),
		imp:      impl,
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  echoMethodHandler,
	}

	req, err := encoding.Marshal(&api.EchoRequest{Message: "ping"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	err = h.Handle(t.Context(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: api.EchoService_Echo_FullMethodName,
		StreamID:       5,
		Timeout:        1000,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Code != uint32(codes.OK) {
		t.Fatalf("code = %d, want OK", header.Code)
	}
	if header.StreamID != 5 {
		t.Fatalf("StreamID = %d, want 5", header.StreamID)
	}
	var reply api.EchoResponse
	if err := encoding.Unmarshal(frames[1], &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "ping" {
		t.Fatalf("reply = %q, want %q", reply.Message, "ping")
	}
}

// TestDefaultMethodHandlerReportsHandlerError is the failure path: a handler
// error becomes a status code in the reply header, and the payload is empty, so
// the connection stays up for the next call.
func TestDefaultMethodHandlerReportsHandlerError(t *testing.T) {
	impl := &testEcho{err: status.Error(codes.InvalidArgument, "bad argument")}
	sink := &frameSink{}
	h := &defaultMethodHandler{
		Sender:   sink,
		Options:  DefaultOptions(),
		imp:      impl,
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  echoMethodHandler,
	}

	req, err := encoding.Marshal(&api.EchoRequest{Message: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	err = h.Handle(t.Context(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: api.EchoService_Echo_FullMethodName,
		StreamID:       6,
		Timeout:        1000,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if codes.Code(header.Code) != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument", codes.Code(header.Code))
	}
	if header.StreamID != 6 {
		t.Fatalf("StreamID = %d, want 6", header.StreamID)
	}
	if header.Desc == "" {
		t.Fatal("Desc empty, want the handler's message")
	}
}

// TestDefaultMethodHandlerAppliesCallTimeout proves the header's timeout
// reaches the handler: one that waits for cancellation must come back as soon
// as the 20ms deadline fires, not hang until the package default does. The
// reply carries Unknown rather than DeadlineExceeded because a handler
// returning the bare ctx.Err() gives the transport no status to report; a
// handler that wants the code has to wrap it itself.
func TestDefaultMethodHandlerAppliesCallTimeout(t *testing.T) {
	impl := &testEcho{gate: make(chan struct{})} // never released: only the deadline can end it
	sink := &frameSink{}
	h := &defaultMethodHandler{
		Sender:   sink,
		Options:  DefaultOptions(),
		imp:      impl,
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  echoMethodHandler,
	}

	req, err := encoding.Marshal(&api.EchoRequest{Message: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	start := time.Now()
	err = h.Handle(t.Context(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: api.EchoService_Echo_FullMethodName,
		StreamID:       7,
		Timeout:        20,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("handler ran %v, want it cut off by the 20ms call timeout", elapsed)
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(sink.Frames()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if got := codes.Code(header.Code); got != codes.Unknown {
		t.Fatalf("code = %v, want Unknown for a bare ctx error", got)
	}
	if !strings.Contains(header.Desc, "deadline exceeded") {
		t.Fatalf("Desc = %q, want the context deadline error", header.Desc)
	}
}

// TestDefaultMethodHandlerPassesMetadata checks that the header's metadata pairs
// are visible to the handler under its own keys.
func TestDefaultMethodHandlerPassesMetadata(t *testing.T) {
	impl := &testEcho{}
	sink := &frameSink{}
	h := &defaultMethodHandler{
		Sender:   sink,
		Options:  DefaultOptions(),
		imp:      impl,
		unaryInt: DefaultServerOptions.unaryInt,
		Handler:  echoMethodHandler,
	}

	req, err := encoding.Marshal(&api.EchoRequest{Message: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	err = h.Handle(t.Context(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: api.EchoService_Echo_FullMethodName,
		Metadatas:      []string{"x-test", "v1"},
		StreamID:       8,
		Timeout:        1000,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	if got := impl.metadataValue("x-test"); got != "v1" {
		t.Fatalf("metadata x-test = %q, want v1", got)
	}
}

// TestEmptyHandlerAnswersUnimplemented covers a method the service does not
// expose: the payload is still consumed, and the reply names the method the
// caller asked for on the stream it came from.
func TestEmptyHandlerAnswersUnimplemented(t *testing.T) {
	sink := &frameSink{}
	e := &empty{Codec: encoding.GetCodec(encoding.Name), Sender: sink}

	req, err := encoding.Marshal(&api.Empty{})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	err = e.Handle(context.Background(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: "/api.EchoService/Missing",
		StreamID:       13,
	}))
	if err != nil {
		t.Fatalf("handle: %v", err)
	}

	frames := sink.Frames()
	if len(frames) != 2 {
		t.Fatalf("frames = %d, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if codes.Code(header.Code) != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", codes.Code(header.Code))
	}
	if header.StreamID != 13 {
		t.Fatalf("StreamID = %d, want 13", header.StreamID)
	}
	if header.Desc != "/api.EchoService/Missing" {
		t.Fatalf("Desc = %q, want the requested method name", header.Desc)
	}
}

// TestServerInterceptorCanWrapTheHandler checks the interceptor chain: it must
// see the request and be able to rewrite the reply.
func TestServerInterceptorCanWrapTheHandler(t *testing.T) {
	sink := &frameSink{}
	impl := &testEcho{}
	wrapped := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		reply, err := handler(ctx, req)
		if err != nil {
			return nil, err
		}
		reply.(*api.EchoResponse).Message = "wrapped:" + reply.(*api.EchoResponse).Message
		return reply, nil
	}
	h := &defaultMethodHandler{
		Sender:   sink,
		Options:  DefaultOptions(),
		imp:      impl,
		unaryInt: wrapped,
		Handler:  echoMethodHandler,
	}

	req, err := encoding.Marshal(&api.EchoRequest{Message: "x"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	if err := h.Handle(t.Context(), newFrameSource(req), WithCallOptions(&api.CallOptions{
		FullMethodName: api.EchoService_Echo_FullMethodName,
		StreamID:       1,
		Timeout:        1000,
	})); err != nil {
		t.Fatalf("handle: %v", err)
	}

	var reply api.EchoResponse
	if err := encoding.Unmarshal(sink.Frames()[1], &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "wrapped:x" {
		t.Fatalf("reply = %q, want the interceptor's rewrite", reply.Message)
	}
}

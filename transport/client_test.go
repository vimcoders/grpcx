package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	grpcmetadata "google.golang.org/grpc/metadata"
)

// requestOn builds the api.Request an Invoke-less caller hands to RoundTrip:
// the method header and the already-marshalled payload.
func requestOn(t *testing.T, method string, msg any) *api.Request {
	t.Helper()
	return &api.Request{
		Options: &api.CallOptions{FullMethodName: method},
		Payload: mustMarshal(t, msg),
	}
}

// TestDialContextReportsAnUnreachableEndpoint covers the dial failure path: the
// error comes back instead of a transport with a dead connection.
func TestDialContextReportsAnUnreachableEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	rt, err := DialContext(ctx, "127.0.0.1:1")
	if err == nil {
		_ = rt.Close()
		t.Fatal("DialContext on a closed port returned no error")
	}
	if rt != nil {
		t.Error("DialContext returned a transport together with an error")
	}
}

// TestInvokeSendsMethodMetadataAndReply covers the happy path: the request
// header names the method and carries the context metadata, and the reply is
// decoded back into the caller's message.
func TestInvokeSendsMethodMetadataAndReply(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	ctx := metadata.AppendToContext(context.Background(), "trace-id", "abc")

	var reply api.EchoResponse
	if err := c.Invoke(ctx, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}

	frames := sender.sent()
	if len(frames) != 2 {
		t.Fatalf("Invoke wrote %d frames, want a header and a payload", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.FullMethodName != "/api.EchoService/Echo" {
		t.Errorf("FullMethodName = %q, want the invoked method", header.FullMethodName)
	}
	if got := joinPairs(header.Metadatas); got != "trace-id=abc" {
		t.Errorf("Metadatas = %q, want trace-id=abc", got)
	}
	if header.StreamID == 0 {
		t.Error("StreamID is zero, so the reply could not be routed")
	}
}

// TestInvokeFoldsTheCallerDeadlineIntoTheHeader covers the deadline path: a
// context deadline shortens the timeout the header carries.
func TestInvokeFoldsTheCallerDeadlineIntoTheHeader(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	var reply api.EchoResponse
	if err := c.Invoke(ctx, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(sender.sent()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Timeout <= 0 || header.Timeout > 100 {
		t.Errorf("header Timeout = %d, want the caller's remaining 1..100ms", header.Timeout)
	}
}

// TestInvokeReturnsTheHandlerError covers a failed call: the status in the reply
// header becomes the returned error.
func TestInvokeReturnsTheHandlerError(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.InvalidArgument, "bad request"))

	var reply api.EchoResponse
	err := c.Invoke(context.Background(), "/m", &api.EchoRequest{}, &reply)
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("Invoke = %v (%v), want InvalidArgument", err, code)
	}
}

// TestInvokeIgnoresGrpcCallOptions pins the current surface: grpc.CallOption
// values are accepted and dropped, so no header metadata reaches the caller.
// It documents a known gap rather than a feature.
func TestInvokeIgnoresGrpcCallOptions(t *testing.T) {
	c, _ := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	var md grpcmetadata.MD

	var reply api.EchoResponse
	err := c.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply, grpc.Header(&md))
	if err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if len(md) != 0 {
		t.Errorf("grpc.Header received %v, want nothing: the option is dropped", md)
	}
}

// TestRoundTripLeavesTheCallerRequestAlone covers a reused request: RoundTrip
// writes its own header, so neither the timeout nor the stream id leaks back
// into the caller's api.CallOptions.
func TestRoundTripLeavesTheCallerRequestAlone(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	req := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"})
	req.Options.Metadatas = []string{"k", "v"}

	for attempt := range 2 {
		if _, err := c.RoundTrip(context.Background(), req); err != nil {
			t.Fatalf("RoundTrip #%d: %v", attempt+1, err)
		}
	}

	if req.Options.Timeout != 0 || req.Options.StreamID != 0 {
		t.Errorf("caller options = (timeout %d, stream %d), want both untouched at 0", req.Options.Timeout, req.Options.StreamID)
	}
	if got := joinPairs(req.Options.Metadatas); got != "k=v" {
		t.Errorf("caller metadata = %q, want it unchanged at k=v", got)
	}

	// The second header proves the first call left nothing behind: neither the
	// stream id nor a timeout grown out of the previous stream. The header carries
	// no timeout here because the caller named none and RoundTrip does not fill in
	// the default itself — the server falls back to its own timeout option.
	frames := sender.sent()
	var second api.CallOptions
	if err := encoding.Unmarshal(frames[2], &second); err != nil {
		t.Fatalf("unmarshal second header: %v", err)
	}
	if second.StreamID != 2 {
		t.Errorf("second StreamID = %d, want 2", second.StreamID)
	}
	if second.Timeout != 0 {
		t.Errorf("second Timeout = %d, want 0: the caller named no timeout", second.Timeout)
	}
}

// TestRoundTripCarriesTheRequestOverTheWire covers the pair of frames RoundTrip
// writes: one call reaches the echo service and its payload comes back.
func TestRoundTripCarriesTheRequestOverTheWire(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	req := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"})

	resp, err := cli.RoundTrip(context.Background(), req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var reply api.EchoResponse
	if err := encoding.Unmarshal(resp.Payload, &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestRoundTripReportsTheStatusInTheReplyHeader covers a failed call: RoundTrip
// must not hand a nil error to a caller whose reply carries a status.
func TestRoundTripReportsTheStatusInTheReplyHeader(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{failOn: "boom"}))
	req := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "boom"})

	resp, err := cli.RoundTrip(context.Background(), req)
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("RoundTrip = (%v, %v), want InvalidArgument", resp, code)
	}
	if resp != nil {
		t.Errorf("RoundTrip returned a response together with the error: %+v", resp)
	}
}

// TestPingTreatsUnimplementedAsAlive covers the health check: the server answers
// an unnamed method with Unimplemented, which proves the connection works.
func TestPingTreatsUnimplementedAsAlive(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Unimplemented, "/api.EchoService/Echo"))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v, want nil for an Unimplemented reply", err)
	}
}

// TestPingPropagatesOtherStatuses covers a reply that means the connection is
// unhealthy.
func TestPingPropagatesOtherStatuses(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Internal, "broken"))
	if code := status.Code(c.Ping(context.Background())); code != codes.Internal {
		t.Errorf("Ping = %v, want Internal", code)
	}
}

// TestPingFailsOnAClosedTransport covers the closed marker: once Close zeroed
// the stream limit, no stream is allocated and the call is Unavailable.
func TestPingFailsOnAClosedTransport(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Unimplemented, ""))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code := status.Code(c.Ping(context.Background())); code != codes.Unavailable {
		t.Errorf("Ping after Close = %v, want Unavailable", code)
	}
}

// TestCreateStreamEnforcesMaxStreams covers the cap: the call past the limit is
// refused with ResourceExhausted.
func TestCreateStreamEnforcesMaxStreams(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithMaxStreams(2))
	ctx := context.Background()

	if _, err := c.createStream(ctx); err != nil {
		t.Fatalf("first stream: %v", err)
	}
	if _, err := c.createStream(ctx); err != nil {
		t.Fatalf("second stream: %v", err)
	}
	if code := status.Code(mustStreamError(t, c)); code != codes.ResourceExhausted {
		t.Errorf("third stream = %v, want ResourceExhausted", code)
	}
}

// TestCreateStreamRefusesAfterClose covers the closed marker.
func TestCreateStreamRefusesAfterClose(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if code := status.Code(mustStreamError(t, c)); code != codes.Unavailable {
		t.Errorf("createStream after Close = %v, want Unavailable", code)
	}
}

// mustStreamError opens a stream and returns the error it failed with, failing
// the test when the stream was granted instead.
func mustStreamError(t *testing.T, c *client) error {
	t.Helper()
	s, err := c.createStream(context.Background())
	if err == nil {
		t.Fatalf("createStream granted stream %d, want an error", s.id)
	}
	return err
}

// TestCloseClosesTheConnectionOnceAndRefusesNewStreams covers the shutdown: the
// connection is closed exactly once and the limit is zeroed so no later stream
// can be created. Close deliberately leaves live streams alone — each one ends
// on its own deadline instead, so a call in flight is never reported as the
// caller cancelling it, and it is not woken early either.
func TestCloseClosesTheConnectionOnceAndRefusesNewStreams(t *testing.T) {
	c, sender := newClientOnSender(t, nil)
	first, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}

	if got := sender.closeCount(); got != 1 {
		t.Errorf("connection closed %d times, want 1", got)
	}
	if first.Context().Err() != nil {
		t.Error("Close canceled a live stream; it is meant to end on its own deadline")
	}
	if _, err := c.createStream(context.Background()); status.Code(err) != codes.Unavailable {
		t.Errorf("createStream after Close = %v, want Unavailable", err)
	}
}

// TestCloseLeavesAnInFlightCallToItsOwnDeadline covers the other half of the
// shutdown: Close closes the connection but does not wake the streams on it, so a
// call in flight keeps waiting and ends on its own deadline with Canceled. The
// code is Canceled on purpose: no reply arrived, so whether the peer ran the
// handler is unknown, and reporting a code callers retry on would invite them to
// replay a call that may already have taken effect.
func TestCloseLeavesAnInFlightCallToItsOwnDeadline(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithTimeout(50*time.Millisecond))
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	start := time.Now()
	done := make(chan error, 1)
	go func() { done <- s.RecvMsg(&api.Empty{}) }()

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if code := status.Code(err); code != codes.Canceled {
			t.Errorf("in-flight call after Close = %v (%v), want Canceled", err, code)
		}
		if d := time.Since(start); d < 30*time.Millisecond {
			t.Errorf("the call ended %v after it started, want it to wait out its 50ms deadline", d)
		}
	case <-time.After(time.Second):
		t.Fatal("the in-flight call never returned")
	}
}

// TestNewStreamIsUnimplemented covers the streaming surface grpcx does not serve.
func TestNewStreamIsUnimplemented(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	s, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/m")
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("NewStream = (%v, %v), want Unimplemented", s, code)
	}
	if s != nil {
		t.Error("NewStream returned a stream although it is unimplemented")
	}
}

// TestInterceptorDiscardsTheFrameOfAGoneStream covers a reply that arrives after
// its stream was closed: both frames are consumed so the connection stays
// aligned, and nothing is reported.
func TestInterceptorDiscardsTheFrameOfAGoneStream(t *testing.T) {
	c := &client{streams: make(map[uint32]*clientStream)}
	recv := &fakeRecver{frames: [][]byte{
		headerFrame(t, &api.CallOptions{StreamID: 99}),
		payloadFrame(t, &api.EchoResponse{Message: "late"}),
	}}

	if err := c.interceptor(context.Background(), recv); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if recv.remaining() != 0 {
		t.Errorf("%d frames left on the connection, want none", recv.remaining())
	}
}

// TestInterceptorRoutesFramesToLiveStreams covers the dispatch: the frame lands
// in the stream its header names.
func TestInterceptorRoutesFramesToLiveStreams(t *testing.T) {
	c := &client{streams: make(map[uint32]*clientStream)}
	s, _ := newStreamUnderTest(&fakeSender{})
	c.streams[s.id] = s
	recv := &fakeRecver{frames: [][]byte{
		headerFrame(t, &api.CallOptions{StreamID: s.id, Code: uint32(codes.OK)}),
		payloadFrame(t, &api.EchoResponse{Message: "hi!"}),
	}}

	if err := c.interceptor(context.Background(), recv); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	select {
	case reply := <-s.c:
		if reply.Options.StreamID != s.id {
			t.Errorf("reply StreamID = %d, want %d", reply.Options.StreamID, s.id)
		}
	default:
		t.Fatal("the frame never reached its stream")
	}
}

// TestHandleStopsWhenTheReceiveLoopContextEnds covers cancellation.
func TestHandleStopsWhenTheReceiveLoopContextEnds(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := c.Handle(ctx, &fakeRecver{})
	if code := status.Code(err); code != codes.Canceled {
		t.Errorf("Handle = %v (%v), want Canceled", err, code)
	}
}

// TestHandleStopsWhenTheConnectionEnds covers a broken connection: the read
// error is returned and the transport is closed behind it.
func TestHandleStopsWhenTheConnectionEnds(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	err := c.Handle(context.Background(), &fakeRecver{})
	if !errors.Is(err, io.EOF) {
		t.Errorf("Handle = %v, want io.EOF", err)
	}
	if c.maxStreams != 0 {
		t.Error("Handle did not close the transport after the connection ended")
	}
}

// TestDialServesCallsOverTCP covers the successful dial path, which net.Pipe
// cannot: Dial builds its own client on a real socket, the receive loop it
// starts there is what hands the reply back, and Close stops it.
func TestDialServesCallsOverTCP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	so := DefaultServerOptions
	so.RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})
	serveCtx, stopServe := context.WithCancel(context.Background())
	defer stopServe()
	handled := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			handled <- acceptErr
			return
		}
		handled <- so.Handle(serveCtx, conn)
	}()

	rt, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}

	var reply api.EchoResponse
	if err := rt.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}

	if err := rt.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
	_ = ln.Close()
	select {
	case serveErr := <-handled:
		if serveErr != nil && !errors.Is(serveErr, io.EOF) {
			t.Logf("server Handle ended with %v after the client closed", serveErr)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the server did not stop within 2s of the client closing")
	}
}

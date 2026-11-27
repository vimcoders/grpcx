package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
	grpcmetadata "google.golang.org/grpc/metadata"
)

// requestOn builds what an Invoke-less caller hands to RoundTrip: the
// already-marshalled payload and the call option naming the method.
func requestOn(t *testing.T, method string, msg any) ([]byte, CallOption) {
	t.Helper()
	return mustMarshal(t, msg), &CallOptions{FullMethodName: method}
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

// TestRoundTripLeavesTheCallerOptionsAlone covers a reused option set:
// RoundTrip builds its own header, so neither the timeout nor the stream id
// leaks back into the caller's CallOptions.
func TestRoundTripLeavesTheCallerOptionsAlone(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	payload := mustMarshal(t, &api.EchoRequest{Message: "hi"})
	opt := &CallOptions{FullMethodName: "/api.EchoService/Echo", Metadatas: []string{"k", "v"}}

	for attempt := range 2 {
		buf, err := c.RoundTrip(context.Background(), payload, opt)
		if err != nil {
			t.Fatalf("RoundTrip #%d: %v", attempt+1, err)
		}
		buf.Free()
	}

	if opt.Timeout != 0 || opt.StreamID != 0 {
		t.Errorf("caller options = (timeout %d, stream %d), want both untouched at 0", opt.Timeout, opt.StreamID)
	}
	if got := joinPairs(opt.Metadatas); got != "k=v" {
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
	if got := joinPairs(second.Metadatas); got != "k=v" {
		t.Errorf("second header metadata = %q, want the option's k=v carried through", got)
	}
}

// TestRoundTripCarriesTheRequestOverTheWire covers the pair of frames RoundTrip
// writes: one call reaches the echo service and its payload comes back.
func TestRoundTripCarriesTheRequestOverTheWire(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	payload, opt := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"})

	buf, err := cli.RoundTrip(context.Background(), payload, opt)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer buf.Free()
	var reply api.EchoResponse
	if err := encoding.Unmarshal(buf.ReadOnlyData(), &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestRoundTripHandsBackAPooledBuffer covers the ownership transfer: a reply
// above the pooling threshold comes back as a reference-counted Buffer, so
// reading it after Free panics instead of showing bytes the pool already took
// back.
func TestRoundTripHandsBackAPooledBuffer(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	body := strings.Repeat("x", 2048)
	payload, opt := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: body})

	buf, err := cli.RoundTrip(context.Background(), payload, opt)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var reply api.EchoResponse
	if err := encoding.Unmarshal(buf.ReadOnlyData(), &reply); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if reply.Message != body+"!" {
		t.Fatalf("reply.Message = %q, want the %d-byte echo", reply.Message, len(body)+1)
	}
	buf.Free()
	defer func() {
		if r := recover(); r == nil {
			t.Error("ReadOnlyData after Free returned instead of panicking: the reply is not reference counted")
		}
	}()
	_ = buf.ReadOnlyData()
}

// TestRoundTripSmallReplyFreesAsANoop covers the lower side of the pool
// threshold: a reply of 256 bytes or less arrives in a buffer whose capacity
// stays under the 1KB pooling threshold, so it comes back as a SliceBuffer whose
// Free is a no-op and whose bytes stay readable.
func TestRoundTripSmallReplyFreesAsANoop(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	payload, opt := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"})

	buf, err := cli.RoundTrip(context.Background(), payload, opt)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if _, ok := buf.(mem.SliceBuffer); !ok {
		t.Fatalf("buffer type = %T, want a SliceBuffer: its capacity stays under the 1KB pooling threshold", buf)
	}
	buf.Free()

	var reply api.EchoResponse
	if err := encoding.Unmarshal(buf.ReadOnlyData(), &reply); err != nil {
		t.Fatalf("read after Free: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestRoundTripReportsTheStatusInTheReplyHeader covers a failed call: RoundTrip
// must not hand a nil error to a caller whose reply carries a status.
func TestRoundTripReportsTheStatusInTheReplyHeader(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{failOn: "boom"}))
	payload, opt := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "boom"})

	buf, err := cli.RoundTrip(context.Background(), payload, opt)
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("RoundTrip = (%v, %v), want InvalidArgument", buf, code)
	}
	if buf != nil {
		t.Errorf("RoundTrip returned a buffer together with the error: %v", buf)
	}
}

// TestRoundTripFoldsTheCallerDeadlineIntoTheHeader covers the timeout the header
// carries: a caller deadline wins over whatever the options name, because the
// header timeout is what bounds the server's handler.
func TestRoundTripFoldsTheCallerDeadlineIntoTheHeader(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	payload := mustMarshal(t, &api.EchoRequest{Message: "hi"})

	ctx, cancel := context.WithTimeout(context.Background(), 750*time.Millisecond)
	defer cancel()
	buf, err := c.RoundTrip(ctx, payload, &CallOptions{Timeout: 9000})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	buf.Free()

	var header api.CallOptions
	if err := encoding.Unmarshal(sender.sent()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if header.Timeout <= 0 || header.Timeout > 750 {
		t.Errorf("Timeout = %d, want the deadline's remaining ms (0 < t <= 750), replacing the option's 9000", header.Timeout)
	}
}

// TestRoundTripCarriesContextMetadataInTheHeader covers the metadata path: the
// header carries what the context holds, the same way Invoke takes it from ctx,
// and the options the caller passed accumulate on top rather than replacing it.
func TestRoundTripCarriesContextMetadataInTheHeader(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	payload := mustMarshal(t, &api.EchoRequest{Message: "hi"})

	ctx := metadata.AppendToContext(context.Background(), "trace-id", "abc")
	buf, err := c.RoundTrip(ctx, payload, &CallOptions{Metadatas: []string{"k", "v"}})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	buf.Free()

	var header api.CallOptions
	if err := encoding.Unmarshal(sender.sent()[0], &header); err != nil {
		t.Fatalf("unmarshal header: %v", err)
	}
	if got := joinPairs(header.Metadatas); got != "k=v,trace-id=abc" {
		t.Errorf("Metadatas = %q, want the option's pair then the context's", got)
	}
}

// TestRoundTripRefusesOnAClosedTransport covers the closed check ahead of stream
// allocation: no header or payload frame is written once the transport is gone.
func TestRoundTripRefusesOnAClosedTransport(t *testing.T) {
	c, sender := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	buf, err := c.RoundTrip(context.Background(), []byte("body"), &CallOptions{FullMethodName: "/api.EchoService/Echo"})
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("RoundTrip = (%v, %v), want Unavailable on a closed transport", buf, code)
	}
	if buf != nil {
		t.Errorf("RoundTrip returned a buffer together with the error: %v", buf)
	}
	if frames := sender.sent(); len(frames) != 0 {
		t.Errorf("sender was given %d frames after Close, want none", len(frames))
	}
}

// TestRoundTripEndsWhenThePeerHangsUp covers a backend that dies mid-call: both
// frames are accepted off the socket and then the connection goes away, so the
// receive loop reports a failure instead of holding the caller until the read
// timeout.
func TestRoundTripEndsWhenThePeerHangsUp(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			return
		}
		// Take what the caller wrote off the socket, then hang up without
		// answering: the receive loop hits the end of the connection.
		_, _ = conn.Read(make([]byte, 1<<16))
		_ = conn.Close()
	}()

	rt, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = rt.Close() }()

	payload, opt := requestOn(t, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"})
	buf, err := rt.RoundTrip(context.Background(), payload, opt)
	if err == nil {
		buf.Free()
		t.Fatal("RoundTrip = nil error, want a failure once the peer hung up")
	}
	if buf != nil {
		t.Errorf("RoundTrip returned a buffer together with the error: %v", buf)
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

// TestPingEndsAtItsOwnBudget covers the budget Ping picks: with a short call
// timeout and a long read timeout, the health check dies at the timeout instead
// of waiting out the reader's budget.
func TestPingEndsAtItsOwnBudget(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithTimeout(50*time.Millisecond), WithReadTimeout(time.Second))
	start := time.Now()
	if err := c.Ping(context.Background()); err == nil {
		t.Fatal("Ping on a silent connection returned nil")
	}
	if d := time.Since(start); d > 300*time.Millisecond {
		t.Errorf("Ping returned after %v, want it cut at its 50ms budget, not the 1s read timeout", d)
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
// can be created. Close cancels the live streams it finds, so a call in flight is
// over as soon as Close returns instead of sitting out its deadline; the wake-up
// is the context cancel, so it reports Canceled.
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
	// Close cancels the stream context, so the call is over as soon as it returns
	// and the context reports it.
	if err := first.Context().Err(); err == nil {
		t.Error("Close did not cancel the live stream context")
	}
	var reply api.Empty
	if code := status.Code(first.RecvMsg(&reply)); code != codes.Canceled {
		t.Errorf("RecvMsg after Close = %v, want Canceled", code)
	}
	if _, err := c.createStream(context.Background()); status.Code(err) != codes.Unavailable {
		t.Errorf("createStream after Close = %v, want Unavailable", err)
	}
}

// TestCloseWakesAnInFlightCall covers the shutdown path: Close walks the stream
// table and cancels each context, so a call waiting on a reply returns at once
// instead of sitting out its deadline. The wake-up is the context cancel, so the
// call reports Canceled; no reply arrived, so whether the peer ran the handler is
// still unknown. A call whose own deadline fired first reports Unavailable, which
// the deadline cases pin separately.
func TestCloseWakesAnInFlightCall(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

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
	case <-time.After(time.Second):
		t.Fatal("the in-flight call did not return after Close")
	}
}

// TestCreateStreamDeadlineFollowsTheReadTimeout pins the wiring today: the
// stream context is cut at the transport's read timeout, so a call with no reply
// ends there and reports Unavailable. WithTimeout no longer reaches this point —
// when createStream starts honouring it again, this case has to change with it.
func TestCreateStreamDeadlineFollowsTheReadTimeout(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithReadTimeout(100*time.Millisecond))
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}

	start := time.Now()
	var reply api.Empty
	err = s.RecvMsg(&reply)
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("RecvMsg after the read timeout = %v (%v), want Unavailable", err, code)
	}
	if d := time.Since(start); d < 50*time.Millisecond {
		t.Errorf("the call ended %v after it started, want it to wait for the 100ms read timeout", d)
	}
}

// TestCreateStreamShortensItsDeadlineToTheCaller pins one half of the min: a
// caller deadline nearer than the read timeout wins, so the stream ends with the
// caller instead of outliving it.
func TestCreateStreamShortensItsDeadlineToTheCaller(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithReadTimeout(time.Second))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	s, err := c.createStream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	dl, ok := s.ctx.Deadline()
	if !ok {
		t.Fatal("the stream context carries no deadline")
	}
	if left := time.Until(dl); left > 80*time.Millisecond {
		t.Errorf("the stream deadline is %v away, want it within the caller's 50ms", left)
	}
}

// TestCreateStreamKeepsTheReadTimeoutWhenTheCallerIsLater pins the other half: a
// caller deadline further out must not widen the stream past the read timeout,
// which is the regression a bare cmp.Or — without the min around it — introduces.
func TestCreateStreamKeepsTheReadTimeoutWhenTheCallerIsLater(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithReadTimeout(100*time.Millisecond))
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	s, err := c.createStream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	dl, ok := s.ctx.Deadline()
	if !ok {
		t.Fatal("the stream context carries no deadline")
	}
	if left := time.Until(dl); left > 150*time.Millisecond {
		t.Errorf("the stream deadline is %v away, want the 100ms read timeout to win", left)
	}
}

// TestCreateStreamIsBornExpiredWhenTheCallerDeadlineHasPassed pins the negative
// branch: min picks the already-expired deadline, so the stream never falls back
// to the read timeout and never waits for one.
func TestCreateStreamIsBornExpiredWhenTheCallerDeadlineHasPassed(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithReadTimeout(100*time.Millisecond))
	ctx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel()
	s, err := c.createStream(ctx)
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	select {
	case <-s.ctx.Done():
	case <-time.After(50 * time.Millisecond):
		t.Fatal("the stream outlived a caller deadline that had already passed")
	}
}

// TestCloseClearsTheStreamTable covers what the receive loop looks up after
// shutdown: Close empties the table, so a late reply frame finds no stream and
// the interceptor reads it off the wire and drops it instead of delivering it to
// a call that is already over.
func TestCloseClearsTheStreamTable(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("stream: %v", err)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := c.getStream(s.id); got != nil {
		t.Errorf("getStream(%d) after Close = %v, want nil", s.id, got)
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

package transport

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// startTestServer serves one EchoService over a loopback listener and returns
// the address to dial. Every accepted connection is handed to Transport.Handle
// in its own goroutine; the cleanup cancels them and waits, so no goroutine
// outlives the test.
func startTestServer(t *testing.T, svc api.EchoServiceServer, opts ...ServerOption) string {
	t.Helper()

	so := DefaultServerOptions
	so.RegisterService(&api.EchoService_ServiceDesc, svc)
	for _, opt := range opts {
		opt(&so)
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		wg.Wait()
	})

	wg.Go(func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			wg.Go(func() {
				defer func() { _ = c.Close() }()
				_ = so.Handle(ctx, c)
			})
		}
	})

	return ln.Addr().String()
}

// dialTestClient connects to addr and closes the transport when the test ends.
// Cleanup is registered after the server's, so it runs first (LIFO) and the
// server's Handle loop sees the connection close instead of waiting for its
// read deadline.
func dialTestClient(t *testing.T, addr string, opts ...Option) RoundTripper {
	t.Helper()

	rt, err := DialContext(context.Background(), addr, opts...)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// waitForCalls blocks until the service has handled want calls, so a test can
// assert on a handler that is still blocked inside Echo.
func waitForCalls(t *testing.T, svc *testEcho, want int) {
	t.Helper()

	deadline := time.Now().Add(2 * time.Second)
	for svc.callCount() < want {
		if time.Now().After(deadline) {
			t.Fatalf("handler reached %d calls, want %d", svc.callCount(), want)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestInvokeEchoRoundTrip is the happy path end to end: the generated stub
// dials through the transport, the server dispatches by method name and the
// reply comes back on the stream the request opened.
func TestInvokeEchoRoundTrip(t *testing.T) {
	svc := &testEcho{}
	addr := startTestServer(t, svc)
	cc := api.NewEchoServiceClient(dialTestClient(t, addr))

	ctx := metadata.WithMetadata(t.Context(), metadata.Pairs("x-test", "v1"))
	reply, err := cc.Echo(ctx, &api.EchoRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := reply.GetMessage(); got != "hello" {
		t.Fatalf("reply = %q, want %q", got, "hello")
	}
	if got := svc.metadataValue("x-test"); got != "v1" {
		t.Fatalf("metadata x-test = %q, want %q", got, "v1")
	}
	if got := svc.callCount(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

// TestInvokeUnknownMethodUnimplemented covers a method the registered service
// does not expose: the server must answer Unimplemented and name the method it
// could not resolve.
func TestInvokeUnknownMethodUnimplemented(t *testing.T) {
	addr := startTestServer(t, &testEcho{})
	rt := dialTestClient(t, addr)

	err := rt.Invoke(t.Context(), "/api.EchoService/Nope", &api.EchoRequest{}, &api.EchoResponse{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented (err = %v)", got, err)
	}
	if !strings.Contains(err.Error(), "Nope") {
		t.Fatalf("error %q does not name the requested method", err)
	}
}

// TestInvokeHandlerErrorPropagates checks that a handler error travels back in
// the response header as a status code instead of breaking the connection.
func TestInvokeHandlerErrorPropagates(t *testing.T) {
	svc := &testEcho{err: status.Error(codes.InvalidArgument, "bad input")}
	addr := startTestServer(t, svc)
	cc := api.NewEchoServiceClient(dialTestClient(t, addr))

	_, err := cc.Echo(t.Context(), &api.EchoRequest{Message: "x"})
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Fatalf("code = %v, want InvalidArgument (err = %v)", got, err)
	}
	if !strings.Contains(err.Error(), "bad input") {
		t.Fatalf("error %q does not carry the handler message", err)
	}
}

// TestInvokeConcurrent runs several goroutines over one connection so the
// stream table, the receive loop and the shared writer are all exercised at
// once. -race is what actually proves it; CI runs that.
func TestInvokeConcurrent(t *testing.T) {
	const goroutines, perGoroutine = 8, 25

	svc := &testEcho{}
	addr := startTestServer(t, svc)
	cc := api.NewEchoServiceClient(dialTestClient(t, addr))

	errs := make(chan error, goroutines*perGoroutine)
	var wg sync.WaitGroup
	for g := range goroutines {
		wg.Go(func() {
			for i := range perGoroutine {
				msg := fmt.Sprintf("g%d-%d", g, i)
				reply, err := cc.Echo(t.Context(), &api.EchoRequest{Message: msg})
				if err != nil {
					errs <- fmt.Errorf("Echo(%s): %w", msg, err)
					return
				}
				if got := reply.GetMessage(); got != msg {
					errs <- fmt.Errorf("reply = %q, want %q", got, msg)
					return
				}
			}
		})
	}
	wg.Wait()
	close(errs)

	for err := range errs {
		t.Error(err)
	}
	if got, want := svc.callCount(), goroutines*perGoroutine; got != want {
		t.Fatalf("handler calls = %d, want %d", got, want)
	}
}

// TestInvokeRespectsCallDeadline checks that a short client deadline ends the
// call promptly. Two outcomes race here and both are correct: the client usually
// notices its own deadline first and reports Canceled, but under load, or with
// the race detector slowing the goroutines down, the server's per-call deadline
// can fire first and send back the handler's bare ctx error, which carries no
// status code and surfaces as Unknown. What must never happen is the call
// waiting out the package default instead of the 50ms header timeout.
func TestInvokeRespectsCallDeadline(t *testing.T) {
	gate := make(chan struct{})
	svc := &testEcho{gate: gate}
	addr := startTestServer(t, svc)
	cc := api.NewEchoServiceClient(dialTestClient(t, addr))
	t.Cleanup(func() { close(gate) })

	ctx, cancel := context.WithTimeout(t.Context(), 50*time.Millisecond)
	defer cancel()

	start := time.Now()
	_, err := cc.Echo(ctx, &api.EchoRequest{Message: "blocked"})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("Echo succeeded, want the deadline to end the call")
	}
	if elapsed > time.Second {
		t.Fatalf("call took %v, want it to end with the 50ms deadline", elapsed)
	}
	if code := status.Code(err); code != codes.Canceled && !strings.Contains(err.Error(), "deadline exceeded") {
		t.Fatalf("code = %v and message %q names no deadline, want Canceled or a deadline error", code, err.Error())
	}
}

// TestCloseUnblocksPendingInvoke checks that closing the transport releases a
// call still waiting for a reply, instead of leaving it until its deadline.
func TestCloseUnblocksPendingInvoke(t *testing.T) {
	gate := make(chan struct{})
	svc := &testEcho{gate: gate}
	addr := startTestServer(t, svc)
	rt := dialTestClient(t, addr)
	cc := api.NewEchoServiceClient(rt)
	t.Cleanup(func() { close(gate) })

	done := make(chan error, 1)
	go func() {
		_, err := cc.Echo(context.Background(), &api.EchoRequest{Message: "pending"})
		done <- err
	}()

	waitForCalls(t, svc, 1)
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-done:
		if got := status.Code(err); got != codes.Canceled {
			t.Fatalf("code = %v, want Canceled (err = %v)", got, err)
		}
	case <-time.After(time.Second):
		t.Fatal("Invoke did not return after Close")
	}
}

// TestPingTreatsUnimplementedAsAlive covers the health check: the ping carries
// no method name, so the server can only answer Unimplemented, and that answer
// proves the connection is up.
func TestPingTreatsUnimplementedAsAlive(t *testing.T) {
	addr := startTestServer(t, &testEcho{})
	rt := dialTestClient(t, addr)

	if err := rt.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestMaxStreamsLimitsConcurrentCalls drives createStream directly: the second
// stream must be refused while the first is open, and allowed again once it is
// released.
func TestMaxStreamsLimitsConcurrentCalls(t *testing.T) {
	addr := startTestServer(t, &testEcho{})
	rt := dialTestClient(t, addr, WithMaxStreams(1))

	c, ok := rt.(*client)
	if !ok {
		t.Fatalf("RoundTripper is %T, want *client", rt)
	}

	first, err := c.createStream(t.Context())
	if err != nil {
		t.Fatalf("first stream: %v", err)
	}
	if _, err := c.createStream(t.Context()); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second stream error = %v, want ResourceExhausted", err)
	}
	_ = first.CloseSend()

	if _, err := c.createStream(t.Context()); err != nil {
		t.Fatalf("stream after release: %v", err)
	}
}

// TestNewStreamUnimplemented pins the documented gap: grpcx serves unary calls
// only, so the streaming entry point refuses every request.
func TestNewStreamUnimplemented(t *testing.T) {
	addr := startTestServer(t, &testEcho{})
	rt := dialTestClient(t, addr)

	if _, err := rt.NewStream(t.Context(), &grpc.StreamDesc{}, api.EchoService_Echo_FullMethodName); status.Code(err) != codes.Unimplemented {
		t.Fatalf("NewStream error = %v, want Unimplemented", err)
	}
}

// TestRoundTripCarriesStreamID covers the pre-framed entry point: the caller
// hands in a header with no stream id, and the reply must still find its way
// back, which only works if the transport stamps the id into the header it
// puts on the wire.
func TestRoundTripCarriesStreamID(t *testing.T) {
	svc := &testEcho{}
	addr := startTestServer(t, svc)
	rt := dialTestClient(t, addr)

	payload, err := encoding.Marshal(&api.EchoRequest{Message: "roundtrip"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	req := &api.Request{
		Options: &api.CallOptions{
			Service:        "api.EchoService",
			FullMethodName: api.EchoService_Echo_FullMethodName,
		},
		Payload: payload,
	}

	reply, err := rt.RoundTrip(t.Context(), req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer putmbuf(reply.Payload)

	var out api.EchoResponse
	if err := encoding.Unmarshal(reply.Payload, &out); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if got := out.GetMessage(); got != "roundtrip" {
		t.Fatalf("reply = %q, want %q", got, "roundtrip")
	}
	if got := svc.callCount(); got != 1 {
		t.Fatalf("handler calls = %d, want 1", got)
	}
}

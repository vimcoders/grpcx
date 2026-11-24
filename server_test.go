package grpcx

import (
	"context"
	"errors"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc/codes"
)

// shortRead is the read deadline every accepted connection gets in these tests.
// A handler parked on an idle connection comes back only when that deadline
// fires, so a short one keeps the shutdown tests fast without changing the
// behavior under test.
const shortRead = 150 * time.Millisecond

// withEcho registers a fresh EchoService implementation on srv.
func withEcho(srv *Server) {
	api.RegisterEchoServiceServer(srv, &echoStub{})
}

func TestServerServesEcho(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	resp, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if resp.Message != "hi!" {
		t.Fatalf("Echo reply = %q, want %q", resp.Message, "hi!")
	}
}

func TestServerReportsUnknownMethod(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	err := cc.Invoke(context.Background(), "/api.EchoService/Nope", &api.EchoRequest{}, &api.EchoResponse{})
	if code := status.Code(err); code != codes.Unimplemented {
		t.Fatalf("Invoke unknown method code = %v (err = %v), want Unimplemented", code, err)
	}
}

// TestServerHandlerErrorStaysOnTheCall pins that a handler error is reported in
// the response status instead of tearing the connection down: the failing call
// carries its code, and the same connection keeps serving.
func TestServerHandlerErrorStaysOnTheCall(t *testing.T) {
	h := startServer(t, func(srv *Server) {
		api.RegisterEchoServiceServer(srv, &echoStub{failOn: "boom"})
	}, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "boom"}); status.Code(err) != codes.Internal {
		t.Fatalf("Echo on a failing handler code = %v (err = %v), want Internal", status.Code(err), err)
	}

	resp, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "ok"})
	if err != nil {
		t.Fatalf("Echo after a handler error: %v", err)
	}
	if resp.Message != "ok!" {
		t.Fatalf("Echo reply = %q, want %q", resp.Message, "ok!")
	}
}

// TestServerClosesIdleConnectionAtReadTimeout pins the read deadline on an idle
// connection: a peer that connects and then says nothing is dropped once the
// deadline fires, which is what frees the handler that owns it.
func TestServerClosesIdleConnectionAtReadTimeout(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(3 * shortRead))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatalf("read on an idle connection returned data, want it closed by the server read deadline")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the test read deadline fired first; the server should close an idle connection after %v", shortRead)
	}
}

func TestServerHandlesConcurrentCalls(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	const calls = 32
	errs := make(chan error, calls)
	var wg sync.WaitGroup
	for range calls {
		wg.Go(func() {
			if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "x"}); err != nil {
				errs <- err
			}
		})
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent Echo: %v", err)
	}
}

// TestServerCancelsServeLoopWithErrClosed pins the shutdown contract: the serve
// loop has no close signal of its own, so a canceled context closes the
// listener through the AfterFunc and the resulting Accept error is what
// ListenAndServe returns.
func TestServerCancelsServeLoopWithErrClosed(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	if err := h.stop(t); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ListenAndServe = %v, want net.ErrClosed", err)
	}
}

// TestServerCancelStopsAccepting checks the other half of that contract: once
// the context is canceled the listener is gone, so dialing the address fails
// instead of being served.
func TestServerCancelStopsAccepting(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	_ = h.stop(t)

	conn, err := net.Dial("tcp", h.addr)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("dial %s after cancel succeeded, want the listener to be closed", h.addr)
	}
}

// TestServerCancelWaitsForHandlers pins that the wait sits at the end of
// ListenAndServe: canceling the context stops new accepts, and the call returns
// only after the in-flight handler is done.
func TestServerCancelWaitsForHandlers(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})

	h := startServer(t, withEcho,
		WithServerReadTimeout(shortRead),
		UnaryServerInterceptor(blockingInterceptor(entered, gate)),
	)
	cc := dialClient(t, h)

	callDone := make(chan error, 1)
	go func() {
		_, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "blocked"})
		callDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("server interceptor did not run within 2s")
	}

	// Cancel without waiting: stop is the helper that waits, and it would block
	// on the parked handler.
	h.cancel()
	select {
	case <-h.serveDone:
		t.Fatalf("ListenAndServe returned while a handler was still in flight")
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	select {
	case <-h.serveDone:
	case <-time.After(2 * time.Second):
		t.Fatalf("ListenAndServe did not return within 2s after the handler finished")
	}
	if err := *h.serveErr; !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ListenAndServe = %v, want net.ErrClosed", err)
	}
	select {
	case err := <-callDone:
		if err != nil {
			t.Logf("the blocked call ended with %v after cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the blocked call did not return within 2s")
	}
}

// TestServerCancelWaitsForIdleHandler covers the cost of that wait on an idle
// connection: canceling does not interrupt a handler parked reading the next
// frame, so ListenAndServe waits out the read deadline.
func TestServerCancelWaitsForIdleHandler(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	// One round trip parks the handler on the next frame.
	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "prime"}); err != nil {
		t.Fatalf("prime call: %v", err)
	}

	start := time.Now()
	if err := h.stop(t); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("ListenAndServe = %v, want net.ErrClosed", err)
	}
	if elapsed := time.Since(start); elapsed < shortRead/2 {
		t.Fatalf("ListenAndServe returned in %v; an idle handler should hold it until the %v read deadline", elapsed, shortRead)
	}
}

// TestServerCloseOnFreshServerReturnsNil covers the documented case: Close does
// nothing, so a Server that never ran ListenAndServe is as fine as a serving one.
func TestServerCloseOnFreshServerReturnsNil(t *testing.T) {
	if err := NewServer().Close(); err != nil {
		t.Fatalf("Close on a fresh server = %v, want nil", err)
	}
}

// TestServerCloseIsIdempotent pins that repeated Close calls all return nil.
// Close is a no-op, so the calls cannot interfere with each other or with the
// serve loop.
func TestServerCloseIsIdempotent(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	for i := range 2 {
		if err := h.srv.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil", i+1, err)
		}
	}
}

// TestServerCloseDoesNotStopAcceptLoop covers what Close deliberately leaves
// alone: it closes nothing, so the listener still answers and a live connection
// is not cut off. The connection opened here is closed right away and unblocks
// the handler with EOF.
func TestServerCloseDoesNotStopAcceptLoop(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	if err := h.srv.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("listener stopped accepting after Close: %v", err)
	}
	_ = conn.Close()
}

// TestServerCloseDoesNotWaitForHandlers pins where the shutdown work moved:
// Close closes nothing and waits for nothing, so it returns at once even with a
// handler in flight. The wait lives at the end of ListenAndServe instead.
func TestServerCloseDoesNotWaitForHandlers(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})

	h := startServer(t, withEcho,
		WithServerReadTimeout(shortRead),
		UnaryServerInterceptor(blockingInterceptor(entered, gate)),
	)
	cc := dialClient(t, h)

	callDone := make(chan error, 1)
	go func() {
		_, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "blocked"})
		callDone <- err
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatalf("server interceptor did not run within 2s")
	}

	start := time.Now()
	if err := h.srv.Close(); err != nil {
		t.Fatalf("Close = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 50*time.Millisecond {
		t.Fatalf("Close took %v with a handler in flight, want it to return at once", elapsed)
	}

	close(gate)
	select {
	case err := <-callDone:
		if err != nil {
			t.Fatalf("the blocked call returned %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("the blocked call did not return within 2s")
	}
}

// TestServerReportsBindError pins that a listen failure is returned as itself,
// not folded into the net.ErrClosed shutdown path.
func TestServerReportsBindError(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if err := NewServer().ListenAndServe(context.Background(), ln.Addr().String()); err == nil {
		t.Fatalf("ListenAndServe on a taken port = nil, want a bind error")
	} else if errors.Is(err, net.ErrClosed) {
		t.Fatalf("bind error = %v, want the listen error rather than net.ErrClosed", err)
	}
}

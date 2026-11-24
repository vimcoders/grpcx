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

	"google.golang.org/grpc"
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
// deadline fires, which is what frees the handler that owns it. The test-side
// deadline is ten times the server one, so a slow runner that delays the handler
// does not make this read fail on the test's own timeout instead.
func TestServerClosesIdleConnectionAtReadTimeout(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	conn, err := net.Dial("tcp", h.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(10 * shortRead))
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

// TestServerCancelReturnsNil pins the shutdown contract: the serve loop has no
// close signal of its own, so a canceled context closes the listener through the
// AfterFunc, and the resulting net.ErrClosed is folded into nil. A cancel is a
// clean stop, not an error the caller has to explain.
func TestServerCancelReturnsNil(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	if err := h.stop(t); err != nil {
		t.Fatalf("ListenAndServe after cancel = %v, want nil", err)
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

// TestServerInterceptorSeesFullMethod pins that a server interceptor installed
// at construction time is applied to every call and receives the full method
// name, which is how middleware dispatches.
func TestServerInterceptorSeesFullMethod(t *testing.T) {
	methods := make(chan string, 1)
	h := startServer(t, withEcho,
		WithServerReadTimeout(shortRead),
		UnaryServerInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			// The handler runs in its own goroutine, so the method name travels
			// back over a channel rather than through a shared variable.
			select {
			case methods <- info.FullMethod:
			default:
			}
			return handler(ctx, req)
		}),
	)
	cc := dialClient(t, h)

	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	select {
	case got := <-methods:
		if got != api.EchoService_Echo_FullMethodName {
			t.Fatalf("server interceptor saw %q, want %q", got, api.EchoService_Echo_FullMethodName)
		}
	case <-time.After(time.Second):
		t.Fatal("server interceptor was not called within 1s")
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
	if err := *h.serveErr; err != nil {
		t.Fatalf("ListenAndServe = %v, want nil", err)
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

// TestServerCancelReturnsWithIdleConnection pins that an idle connection cannot
// keep ListenAndServe from returning forever. There are two legitimate paths:
// the handler notices the cancel at the top of its next loop turn and returns at
// once, or it is already parked in a read and rides out the deadline that call
// armed. Requiring the second one on every run is not a property, so the check
// here is the bound instead.
func TestServerCancelReturnsWithIdleConnection(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	// One round trip parks the handler on the next frame.
	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "prime"}); err != nil {
		t.Fatalf("prime call: %v", err)
	}

	start := time.Now()
	if err := h.stop(t); err != nil {
		t.Fatalf("ListenAndServe = %v, want nil", err)
	}
	if elapsed := time.Since(start); elapsed > 8*shortRead {
		t.Fatalf("ListenAndServe took %v with an idle connection, want it back within %v", elapsed, 8*shortRead)
	}
}

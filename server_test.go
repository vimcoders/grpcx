package grpcx

import (
	"context"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

// shortRead is the server read deadline used here: short enough to keep the idle
// connection test quick, long enough not to fire while a message is in flight.
const shortRead = 150 * time.Millisecond

// stop cancels the server and waits for ListenAndServe to return, which is when
// every accepted connection has been served to its end.
func (ts *testServer) stop(t *testing.T) error {
	t.Helper()
	ts.cancel()
	select {
	case err := <-ts.done:
		return err
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return within 2s of the cancel")
		return nil
	}
}

// blockingInterceptor parks every call on gate after reporting that it entered,
// so a test can hold a call in flight while it cancels the server. It ignores
// the context on purpose: the handler has to be released by the test, not by the
// cancellation.
func blockingInterceptor(entered chan<- struct{}, gate <-chan struct{}) grpc.UnaryServerInterceptor {
	return func(_ context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return handler(context.Background(), req)
	}
}

// TestServerOptionsReachTheTransport covers the five server options at the root
// package: each one is a thin forwarder to transport, and the check here is that
// a server built with all of them still serves, and that the read timeout
// actually reaches an accepted connection.
func TestServerOptionsReachTheTransport(t *testing.T) {
	intercepted := make(chan struct{}, 1)
	srv := serve(t,
		WithServerCodec(encoding.GetCodec(encoding.Name)),
		WithServerReadTimeout(shortRead),
		WithServerWriteTimeout(shortRead),
		WithServerBufSize(2),
		UnaryServerInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			select {
			case intercepted <- struct{}{}:
			default:
			}
			return handler(ctx, req)
		}),
	)

	cc, err := Dial(srv.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	var reply api.EchoResponse
	if callErr := cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); callErr != nil {
		t.Fatalf("Invoke: %v", callErr)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
	select {
	case <-intercepted:
	case <-time.After(time.Second):
		t.Fatal("the server interceptor never ran")
	}

	// WithServerReadTimeout reaches the accepted connection: a peer that says
	// nothing is dropped once the deadline fires. The test-side deadline is ten
	// times the server one so a slow runner fails on the wrong side otherwise.
	conn, err := net.Dial("tcp", srv.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", srv.addr, err)
	}
	defer func() { _ = conn.Close() }()

	_ = conn.SetReadDeadline(time.Now().Add(10 * shortRead))
	if _, err := conn.Read(make([]byte, 1)); err == nil {
		t.Fatal("read on an idle connection returned data, want it closed by the read deadline")
	} else if errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("the test read deadline fired first; the server should close an idle connection after %v", shortRead)
	}
}

// TestListenAndServeWaitsForTheInFlightCall covers the wait at the end of the
// accept loop: canceling the context stops new accepts, and the call returns
// only after the handler that was already running is done.
func TestListenAndServeWaitsForTheInFlightCall(t *testing.T) {
	entered := make(chan struct{}, 1)
	gate := make(chan struct{})
	srv := serve(t, UnaryServerInterceptor(blockingInterceptor(entered, gate)))

	// The transport leaves live streams alone when a connection goes down, so the
	// call below can only end once its own deadline fires: keep that deadline
	// short instead of waiting out the 3s default.
	cc, err := Dial(srv.addr, WithTimeout(500*time.Millisecond))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	callDone := make(chan error, 1)
	go func() {
		var reply api.EchoResponse
		callDone <- cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "blocked"}, &reply)
	}()
	select {
	case <-entered:
	case <-time.After(2 * time.Second):
		t.Fatal("the handler did not start within 2s")
	}

	// Cancel without waiting for the outcome: a return here would mean the
	// server gave up on the handler still parked on gate.
	srv.cancel()
	select {
	case early := <-srv.done:
		t.Fatalf("ListenAndServe returned (%v) while a handler was still in flight", early)
	case <-time.After(50 * time.Millisecond):
	}

	close(gate)
	select {
	case err := <-srv.done:
		if err != nil {
			t.Fatalf("ListenAndServe = %v, want nil after its own shutdown", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return within 2s after the handler finished")
	}
	select {
	case err := <-callDone:
		if err != nil {
			t.Logf("the blocked call ended with %v after the cancel", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the blocked call did not return within 2s")
	}
}

// TestListenAndServeStopsAcceptingAfterCancel covers the other half of shutdown:
// once ListenAndServe has returned, the listener is closed, so a dial fails
// instead of being accepted and served by nothing.
func TestListenAndServeStopsAcceptingAfterCancel(t *testing.T) {
	srv := serve(t)
	if err := srv.stop(t); err != nil {
		t.Fatalf("ListenAndServe = %v, want nil after its own shutdown", err)
	}

	conn, err := net.DialTimeout("tcp", srv.addr, 100*time.Millisecond)
	if err == nil {
		_ = conn.Close()
		t.Fatalf("dial %s after shutdown succeeded, want the listener closed", srv.addr)
	}
}

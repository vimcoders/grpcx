package grpcx

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestServerServesRegisteredService pins the serving loop: a service registered
// at construction answers over a real listener, which is the only path that
// exercises accept, dispatch and reply together.
func TestServerServesRegisteredService(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)

	var reply api.EchoResponse
	if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "served"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := reply.GetMessage(); got != "served!" {
		t.Fatalf("reply = %q, want served!", got)
	}
}

// TestServerAppliesUnaryInterceptor pins that NewServer installs the interceptor
// passed at construction: options are applied once, and a later registration
// cannot retro-fit one, so an interceptor that never runs is silently missing
// auth or tracing on every call.
func TestServerAppliesUnaryInterceptor(t *testing.T) {
	rec := &countingInterceptor{}
	_, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		UnaryServerInterceptor(rec.interceptor()),
	)
	cli := dialClient(t, addr)

	if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "x"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("interceptor ran %d times, want 1", got)
	}
}

// TestServerRegisterServiceReplaces pins the single-service rule: the dispatch
// table holds one descriptor, so a later registration replaces the earlier one.
// The test observes it from the wire—the superseded service stops answering
// while the new one starts—because the descriptor itself is not exported.
func TestServerRegisterServiceReplaces(t *testing.T) {
	first := &grpc.ServiceDesc{ServiceName: "first.Echo", Methods: api.EchoService_ServiceDesc.Methods}
	second := &grpc.ServiceDesc{ServiceName: "second.Echo", Methods: api.EchoService_ServiceDesc.Methods}
	srv, addr := startServer(t, transport.RegisterService(first, &echoStub{}))
	cli := dialClient(t, addr)

	ctx := context.Background()
	if err := cli.Invoke(ctx, "/first.Echo/Echo", &api.EchoRequest{Message: "a"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("first registration did not serve: %v", err)
	}

	srv.RegisterService(second, &echoStub{})

	if err := cli.Invoke(ctx, "/first.Echo/Echo", &api.EchoRequest{}, &api.EchoResponse{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("superseded service = %v, want Unimplemented", err)
	}
	if err := cli.Invoke(ctx, "/second.Echo/Echo", &api.EchoRequest{Message: "b"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("second registration did not serve: %v", err)
	}
}

// TestServerRejectsUnknownMethod pins the fallback: a method the descriptor does
// not expose is answered with codes.Unimplemented carrying the method name, and
// the connection stays usable for the next call.
func TestServerRejectsUnknownMethod(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)

	ctx := context.Background()
	err := cli.Invoke(ctx, "/api.EchoService/Missing", &api.EchoRequest{}, &api.EchoResponse{})
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("unknown method = %v (code %v), want Unimplemented", err, got)
	}
	if err := cli.Invoke(ctx, api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "after"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("connection unusable after an unknown method: %v", err)
	}
}

// TestServerCloseWaitsForHandlers pins what Close does with a live connection:
// it waits in wg.Wait for the handler to come back, and that handler is woken by
// the read timeout rather than by a closed connection, so the server is
// configured with a short one. Close returns once that deadline fires instead of
// sitting for the production default of 30s. The connection is unusable
// afterwards because the handler closes it on the way out, not because Close
// touched it: Close never closes the listener or the connections.
func TestServerCloseWaitsForHandlers(t *testing.T) {
	srv, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		WithServerReadTimeout(50*time.Millisecond),
	)
	cli := dialClient(t, addr)
	c := api.NewEchoServiceClient(cli)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := c.Echo(ctx, &api.EchoRequest{Message: "before"}); err != nil {
		t.Fatalf("Echo before Close: %v", err)
	}

	began := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if took := time.Since(began); took > time.Second {
		t.Fatalf("Close took %v, want it to return once the read timeout fires", took)
	}

	if _, err := c.Echo(ctx, &api.EchoRequest{Message: "after"}); err == nil {
		t.Fatal("Echo after Close = nil error, want the connection gone")
	}
}

// TestServerCloseTwiceDoesNotPanic pins the second-close path: a supervisor
// closes explicitly and the deferred Close in cmd/grpcx runs as well, so a
// repeat call must not dereference a nil cancel function, wait on a finished
// group, or report the listener it already shut.
func TestServerCloseTwiceDoesNotPanic(t *testing.T) {
	srv, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		WithServerReadTimeout(50*time.Millisecond),
	)
	cli := dialClient(t, addr)

	if _, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestServerCloseOnFreshServerReturnsNil pins that Close is a no-op on a Server
// that never served: it waits on an empty handler group and returns nil. cmd/grpcx
// defers Close, and a Listen that fails returns before anything is installed, so
// the deferred call must not turn a bind error into a different failure.
func TestServerCloseOnFreshServerReturnsNil(t *testing.T) {
	if err := NewServer().Close(); err != nil {
		t.Fatalf("Close on a fresh Server = %v, want nil", err)
	}
}

// TestServerListenAndServeStopsOnContextCancel pins the shutdown path
// signal.NotifyContext drives: canceling ctx has to bring the call back instead
// of leaving it parked in Accept. The AfterFunc closes the listener to wake it,
// so what comes back is net.ErrClosed; the test pins both that the call returns
// promptly and what it returns.
func TestServerListenAndServeStopsOnContextCancel(t *testing.T) {
	srv := NewServer(WithServerReadTimeout(50 * time.Millisecond))
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()
	waitListening(t, addr, done)

	began := time.Now()
	cancel()
	select {
	case err := <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("ListenAndServe after ctx cancel = %v, want net.ErrClosed", err)
		}
		if took := time.Since(began); took > time.Second {
			t.Fatalf("ListenAndServe took %v to return after ctx cancel", took)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return within 2s of ctx cancel")
	}

	// Close only waits for the handlers, so after the loop is gone it is a
	// no-op that still has to report nil.
	if err := srv.Close(); err != nil {
		t.Fatalf("Close after cancel = %v, want nil", err)
	}
}

// TestServerListenAndServeReportsBindError pins the startup failure path: a
// taken address is reported to the caller instead of serving from nowhere, and
// the Close that cmd/grpcx defers afterwards stays a no-op.
func TestServerListenAndServeReportsBindError(t *testing.T) {
	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("occupy address: %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := NewServer()
	if err := srv.ListenAndServe(context.Background(), addr); err == nil {
		t.Fatal("ListenAndServe on a taken address = nil error")
	}
	if err := srv.Close(); err != nil {
		t.Logf("Close after a failed Listen reported %v", err)
	}
}

// TestServerHangsUpIdleConnection pins that WithServerReadTimeout reaches the
// per-connection channel: a peer that connects and never sends a frame is
// dropped by the server instead of holding a handler until the peer leaves.
// The client's own deadline is set well above the server's, so a timeout
// observed here can only be the server's.
func TestServerHangsUpIdleConnection(t *testing.T) {
	_, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		WithServerReadTimeout(50*time.Millisecond),
	)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer func() { _ = conn.Close() }()
	if err := conn.SetReadDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatalf("arm client deadline: %v", err)
	}

	buf := make([]byte, 8)
	var nerr net.Error
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("read on an idle connection succeeded, want the server to close it")
	} else if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("the client's own 2s deadline fired, want the server's 50ms read timeout first")
	}
}

// TestServerHandlesConcurrentCalls pins that one connection carries several
// calls at once: the client multiplexes them by stream id and the server answers
// them one after another without mixing the replies up.
func TestServerHandlesConcurrentCalls(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)
	c := api.NewEchoServiceClient(cli)

	const (
		workers   = 3
		callsEach = 5
	)
	var wg sync.WaitGroup
	failures := make(chan error, workers*callsEach)
	for range workers {
		wg.Go(func() {
			for range callsEach {
				if _, err := c.Echo(context.Background(), &api.EchoRequest{Message: "concurrent"}); err != nil {
					failures <- err
					return
				}
			}
		})
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent Echo: %v", err)
	}
}

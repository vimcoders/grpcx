package grpcx

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// startHandlingServer accepts connections on a loopback listener and serves
// each one with srv.Handle, the same per-connection entry point ListenAndServe
// uses.
func startHandlingServer(t *testing.T, srv *Server) net.Listener {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
	})

	go func() {
		for {
			conn, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			go func() { _ = srv.Handle(ctx, conn) }()
		}
	}()
	return ln
}

// TestServerServesRegisteredService runs a full call: client dial, balancer
// pick, connection, codec, interceptor, handler and reply.
func TestServerServesRegisteredService(t *testing.T) {
	codec := &countingCodec{enc: encoding.GetCodec(encoding.Name)}
	echo := &echoServer{}
	var interceptorCalls atomic.Int64

	srv := NewServer(
		WithServerCodec(codec),
		UnaryServerInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			interceptorCalls.Add(1)
			return handler(ctx, req)
		}),
	)
	api.RegisterEchoServiceServer(srv, echo)
	ln := startHandlingServer(t, srv)

	cli, err := Dial(ln.Addr().String(), WithCodec(codec))
	if err != nil {
		t.Fatalf("Dial(%s) = %v", ln.Addr(), err)
	}
	defer func() {
		if closeErr := cli.Close(); closeErr != nil {
			t.Fatalf("client Close = %v", closeErr)
		}
	}()

	for i, msg := range []string{"ping", "pong"} {
		reply := new(api.EchoResponse)
		if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: msg}, reply); err != nil {
			t.Fatalf("Invoke %d = %v", i, err)
		}
		if got := reply.GetMessage(); got != msg {
			t.Fatalf("reply %d = %q, want %q", i, got, msg)
		}
	}

	if got := echo.callCount(); got != 2 {
		t.Fatalf("handler calls = %d, want 2", got)
	}
	if got := interceptorCalls.Load(); got != 2 {
		t.Fatalf("interceptor calls = %d, want 2", got)
	}
	// Two calls each way: request marshal plus reply marshal on both sides.
	if got := codec.marshals.Load(); got < 4 {
		t.Fatalf("codec marshals = %d, want at least 4", got)
	}
	if got := codec.unmarshals.Load(); got < 4 {
		t.Fatalf("codec unmarshals = %d, want at least 4", got)
	}
}

// TestServerReturnsHandlerError checks that a failing handler reaches the caller
// as a status error with its original code.
func TestServerReturnsHandlerError(t *testing.T) {
	echo := &echoServer{err: status.NotFound.Err()}
	srv := NewServer()
	api.RegisterEchoServiceServer(srv, echo)
	ln := startHandlingServer(t, srv)

	cli, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial = %v", err)
	}
	defer func() { _ = cli.Close() }()

	err = cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "x"}, new(api.EchoResponse))
	if err == nil {
		t.Fatalf("Invoke against a failing handler = nil error")
	}
	if got := status.Code(err); got != codes.NotFound {
		t.Fatalf("error code = %v, want NotFound (err = %v)", got, err)
	}
}

// TestListenAndServeShutsDownOnClose covers the serving loop: it accepts on the
// configured address, serves the call, and returns once Close shuts it down.
func TestListenAndServeShutsDownOnClose(t *testing.T) {
	addr := reserveAddr(t)
	srv := NewServer()
	api.RegisterEchoServiceServer(srv, &echoServer{})

	errCh := make(chan error, 1)
	go func() { errCh <- srv.ListenAndServe(context.Background(), addr) }()

	var cli ClientConnInterface
	deadline := time.Now().Add(5 * time.Second)
	var dialErr error
	for time.Now().Before(deadline) {
		cli, dialErr = Dial(addr)
		if dialErr == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if cli == nil {
		t.Fatalf("server never became dialable on %s: %v", addr, dialErr)
	}

	reply := new(api.EchoResponse)
	if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hello"}, reply); err != nil {
		t.Fatalf("Invoke = %v", err)
	}
	if got := reply.GetMessage(); got != "hello" {
		t.Fatalf("reply = %q, want hello", got)
	}

	_ = cli.Close()
	started := time.Now()
	if err := srv.Close(); err != nil {
		t.Fatalf("Server.Close = %v", err)
	}
	// Prompt shutdown: Close closes the open connections, so the handler must
	// not wait out its read timeout (30s) before returning.
	if elapsed := time.Since(started); elapsed > 2*time.Second {
		t.Fatalf("Close took %v, want a prompt shutdown", elapsed)
	}
	select {
	case err := <-errCh:
		if err == nil {
			t.Fatalf("ListenAndServe returned nil after the listener was closed")
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("ListenAndServe did not return after Close")
	}
}

// TestListenAndServeReportsListenError checks the bind failure path.
func TestListenAndServeReportsListenError(t *testing.T) {
	srv := NewServer()
	if err := srv.ListenAndServe(context.Background(), "256.256.256.256:0"); err == nil {
		t.Fatalf("ListenAndServe on an invalid address = nil error")
	}
}

func TestCloseIsIdempotent(t *testing.T) {
	srv := NewServer()
	for i := range 2 {
		if err := srv.Close(); err != nil {
			t.Fatalf("Close %d = %v", i, err)
		}
	}
}

// TestServerOptionWrappers covers the constructors: each returns an option that
// can be applied to the defaults without panicking.
func TestServerOptionWrappers(t *testing.T) {
	opts := []ServerOption{
		UnaryServerInterceptor(func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			return handler(ctx, req)
		}),
		WithServerCodec(encoding.GetCodec(encoding.Name)),
		WithServerReadTimeout(time.Second),
		WithServerWriteTimeout(time.Second),
		WithServerBufSize(2),
	}

	so := transport.DefaultServerOptions
	for i, opt := range opts {
		if opt == nil {
			t.Fatalf("option %d is nil", i)
		}
		opt(&so)
	}
}

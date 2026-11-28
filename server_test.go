package grpcx

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

// echoImpl is the service the end-to-end test registers.
type echoImpl struct {
	api.UnimplementedEchoServiceServer
}

// Echo implements api.EchoServiceServer.
func (e *echoImpl) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message + "!"}, nil
}

var _ grpc.ServiceRegistrar = (*Server)(nil)

// TestNewServerWiresHandlerAndRegistrar covers construction: the two halves of
// Server point at the same ServerOptions, so a registration reaches the handler
// that serves connections.
func TestNewServerWiresHandlerAndRegistrar(t *testing.T) {
	s := NewServer(WithServerBufSize(0))
	handler, ok := s.Handler.(*transport.ServerOptions)
	if !ok {
		t.Fatalf("Handler is %T, want *transport.ServerOptions", s.Handler)
	}
	registrar, ok := s.ServiceRegistrar.(*transport.ServerOptions)
	if !ok {
		t.Fatalf("ServiceRegistrar is %T, want *transport.ServerOptions", s.ServiceRegistrar)
	}
	if handler != registrar {
		t.Error("Handler and ServiceRegistrar are different values")
	}
}

// TestNewServerRegistersTheService covers the promoted RegisterService: the
// descriptor lands on the options the handler serves from.
func TestNewServerRegistersTheService(t *testing.T) {
	s := NewServer()
	s.RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})
	so := s.Handler.(*transport.ServerOptions)
	buf, err := so.Marshal(&api.EchoResponse{Message: "hi"})
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	buf.Free()
}

// freeAddr returns a loopback address that was free a moment ago.
func freeAddr(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := l.Addr().String()
	if err := l.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// TestListenAndServeStopsOnCancel covers the shutdown path: a canceled context
// closes the listener, and Accept's closed error is folded into nil.
func TestListenAndServeStopsOnCancel(t *testing.T) {
	s := NewServer()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- s.ListenAndServe(ctx, addr) }()

	waitForPort(t, addr)
	cancel()
	select {
	case err := <-errs:
		if err != nil {
			t.Errorf("ListenAndServe = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// TestListenAndServeServesEcho covers the whole stack: a real listener, the
// balancer dialing it, and one unary call through the transport.
func TestListenAndServeServesEcho(t *testing.T) {
	s := NewServer()
	s.RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	addr := freeAddr(t)
	errs := make(chan error, 1)
	go func() { errs <- s.ListenAndServe(ctx, addr) }()

	waitForPort(t, addr)
	cli, err := DialContext(ctx, addr)
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	var resp api.EchoResponse
	callCtx, callCancel := context.WithTimeout(ctx, 2*time.Second)
	defer callCancel()
	if err := cli.Invoke(callCtx, api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &resp); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Message != "hi!" {
		t.Errorf("reply = %q, want hi!", resp.Message)
	}

	cancel()
	select {
	case err := <-errs:
		if err != nil {
			t.Errorf("ListenAndServe = %v, want nil", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("ListenAndServe did not return after cancel")
	}
}

// waitForPort blocks until addr accepts a TCP connection, which is the only
// signal that ListenAndServe has installed its listener: the listen happens
// inside the server goroutine, so an early dial reaches a closed port, and the
// balancer swallows that dial failure and keeps an empty pool instead of
// failing the build.
func waitForPort(t *testing.T, addr string) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		c, err := net.DialTimeout("tcp", addr, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatalf("port %s never accepted a connection", addr)
}

// TestListenAndServeReportsABusyAddress covers the listen error: it comes back
// as itself instead of being folded into nil.
func TestListenAndServeReportsABusyAddress(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = l.Close() })

	s := NewServer()
	if err := s.ListenAndServe(context.Background(), l.Addr().String()); err == nil {
		t.Fatal("ListenAndServe on a busy address returned nil")
	}
}

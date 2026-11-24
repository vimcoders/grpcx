package grpcx

import (
	"context"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// echoStub is the api.EchoServiceServer implementation these tests register.
// It answers with the request message plus "!", and fails the calls carrying
// failOn so a test can pin how a handler error reaches the client.
type echoStub struct {
	api.UnimplementedEchoServiceServer

	failOn string
}

// Echo implements api.EchoServiceServer.
func (s *echoStub) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	if s.failOn != "" && req.Message == s.failOn {
		return nil, status.Error(codes.Internal, "test: handler failed")
	}
	return &api.EchoResponse{Message: req.Message + "!"}, nil
}

// serveHandle is a Server whose accept loop runs in the background. serveDone
// is closed once ListenAndServe returns and serveErr holds its result. Closing
// the channel, rather than sending on it, lets every observer read the outcome
// without consuming it, so the readiness probe and the cleanup can both see it.
type serveHandle struct {
	srv       *Server
	addr      string
	cancel    context.CancelFunc
	serveDone chan struct{}
	serveErr  *error
}

// freeAddr reserves a loopback address and releases it, so the server can bind
// it a moment later. ListenAndServe does not report the port it bound, and a
// test needs the address before the call blocks.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve address: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release address: %v", err)
	}
	return addr
}

// startServer runs a Server on a reserved loopback address. register, when not
// nil, installs the service before the loop starts.
//
// Cleanup cancels the serve loop and waits for ListenAndServe to return. Close
// is called as well, because it is how a server is shut down, but it is a no-op:
// the handler group now lives inside ListenAndServe, whose deferred wait runs
// once the accept loop has exited, so its Wait can never overlap an Add from a
// freshly accepted connection. A test that stops the loop itself with stop can
// still rely on this cleanup, because cancel is idempotent and the done channel
// is closed.
func startServer(t *testing.T, register func(*Server), opts ...ServerOption) *serveHandle {
	t.Helper()
	srv := NewServer(opts...)
	if register != nil {
		register(srv)
	}
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	var serveErr error
	go func() {
		serveErr = srv.ListenAndServe(ctx, addr)
		close(done)
	}()
	h := &serveHandle{srv: srv, addr: addr, cancel: cancel, serveDone: done, serveErr: &serveErr}
	h.waitListening(t)
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Log("ListenAndServe did not return within 2s")
		}
		_ = srv.Close()
	})
	return h
}

// waitListening blocks until addr accepts connections. freeAddr only reserves
// the port and releases it, and ListenAndServe binds inside its own goroutine,
// so a dial issued right after startServer can reach a port nobody listens on
// yet.
//
// The probe stays open until the server closes it. A probe that is simply
// closed can sit in the backlog while the test moves on, and the handler for it
// would then still be running, outliving the assumptions a case makes about how
// many handlers are in flight. Reading to EOF proves the accept loop already
// consumed the probe and its handler ended on the read deadline, so the backlog
// is empty when the probe returns.
func (h *serveHandle) waitListening(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("tcp", h.addr)
		if err == nil {
			_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
			_, _ = io.Copy(io.Discard, conn)
			_ = conn.Close()
			return
		}
		select {
		case <-h.serveDone:
			t.Fatalf("ListenAndServe on %s returned before it listened: %v", h.addr, *h.serveErr)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("address %s did not start listening within 2s", h.addr)
		}
		time.Sleep(time.Millisecond)
	}
}

// stop cancels the serve loop and returns the error ListenAndServe ended with.
// The listener is closed through the context, so a canceled loop reports
// net.ErrClosed rather than nil. ListenAndServe waits for its handlers before it
// returns, so this call also means every handler has finished.
func (h *serveHandle) stop(t *testing.T) error {
	t.Helper()
	h.cancel()
	select {
	case <-h.serveDone:
		return *h.serveErr
	case <-time.After(2 * time.Second):
		t.Fatalf("ListenAndServe did not return within 2s")
		return nil
	}
}

// dialClient builds a client for h. Its cleanup runs before the server cleanup
// registered above, because cleanups run last in first out: the connection is
// closed before the server starts waiting on its handlers.
func dialClient(t *testing.T, h *serveHandle) ClientConnInterface {
	t.Helper()
	cc, err := Dial(h.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })
	return cc
}

// echoClient returns the generated EchoService client over cc.
func echoClient(cc ClientConnInterface) api.EchoServiceClient {
	return api.NewEchoServiceClient(cc)
}

// blockingInterceptor reports that it was entered and then parks until gate is
// closed, so a test can hold one handler in flight.
func blockingInterceptor(entered chan<- struct{}, gate <-chan struct{}) grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, _ *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		select {
		case entered <- struct{}{}:
		default:
		}
		<-gate
		return handler(ctx, req)
	}
}

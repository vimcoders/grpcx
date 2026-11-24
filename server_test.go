package grpcx

import (
	"context"
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

// countingInterceptor records how often the server interceptor ran, so a test
// can assert the option installed at construction time is the one in effect.
type countingInterceptor struct {
	mu sync.Mutex
	n  int
}

func (c *countingInterceptor) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		c.mu.Lock()
		c.n++
		c.mu.Unlock()
		return handler(ctx, req)
	}
}

func (c *countingInterceptor) count() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.n
}

// TestServerListenAndServeServes pins the serving loop: a registered service
// answers over a real listener, which is the only path that exercises accept,
// dispatch and reply together.
func TestServerListenAndServeServes(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)

	reply, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "served"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if reply.Message != "served!" {
		t.Fatalf("reply = %q, want served!", reply.Message)
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

	if _, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := rec.count(); got != 1 {
		t.Fatalf("interceptor ran %d times, want 1", got)
	}
}

// TestServerRegisterServiceReplaces pins the single-service rule: the dispatch
// table holds one descriptor, so a later registration replaces the earlier one.
// The test observes it from the wire — the superseded service stops answering —
// because the descriptor itself is not exported.
func TestServerRegisterServiceReplaces(t *testing.T) {
	first := &grpc.ServiceDesc{ServiceName: "first.Echo", Methods: api.EchoService_ServiceDesc.Methods}
	second := &grpc.ServiceDesc{ServiceName: "second.Echo", Methods: api.EchoService_ServiceDesc.Methods}
	srv, addr := startServer(t, transport.RegisterService(first, &echoStub{}))
	cli := dialClient(t, addr)

	if err := cli.Invoke(context.Background(), "/first.Echo/Echo", &api.EchoRequest{Message: "a"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("first registration did not serve: %v", err)
	}

	srv.RegisterService(second, &echoStub{})

	if err := cli.Invoke(context.Background(), "/first.Echo/Echo", &api.EchoRequest{}, &api.EchoResponse{}); status.Code(err) != codes.Unimplemented {
		t.Fatalf("superseded service = %v, want Unimplemented", err)
	}
	if err := cli.Invoke(context.Background(), "/second.Echo/Echo", &api.EchoRequest{Message: "b"}, &api.EchoResponse{}); err != nil {
		t.Fatalf("second registration did not serve: %v", err)
	}
}

// TestServerCloseIsIdempotent pins that Close can run twice: ListenAndServe
// closes on the way out and a test or a supervisor closes explicitly, so a
// second call must not touch a closed listener or wait on an empty group.
func TestServerCloseIsIdempotent(t *testing.T) {
	srv := NewServer()

	if err := srv.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestServerTrackConnRejectsAfterClose pins the admission check: a connection
// accepted after Close must be refused, because nothing would ever close it
// again and the handler would outlive the server.
func TestServerTrackConnRejectsAfterClose(t *testing.T) {
	srv := NewServer()
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	conn, peer := net.Pipe()
	defer func() {
		_ = conn.Close()
		_ = peer.Close()
	}()

	if srv.trackConn(conn) {
		t.Fatal("trackConn accepted a connection after Close")
	}
}

// TestServerCloseStopsServing pins that Close tears down a live connection:
// closing the listener alone leaves the handler parked in a read, and the client
// would keep using a socket nothing serves. After Close the next call has to
// fail instead of hanging.
func TestServerCloseStopsServing(t *testing.T) {
	srv, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)
	client := api.NewEchoServiceClient(cli)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if _, err := client.Echo(ctx, &api.EchoRequest{Message: "before"}); err != nil {
		t.Fatalf("Echo before Close: %v", err)
	}
	if err := srv.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if _, err := client.Echo(ctx, &api.EchoRequest{Message: "after"}); err == nil {
		t.Fatal("Echo after Close = nil error, want the connection refused")
	}
}

// TestServerListenAndServeReportsBindError pins the startup failure path: a
// taken address is reported to the caller instead of serving from nowhere.
func TestServerListenAndServeReportsBindError(t *testing.T) {
	addr := freeAddr(t)
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		t.Fatalf("occupy address: %v", err)
	}
	defer func() { _ = ln.Close() }()

	if err := NewServer().ListenAndServe(context.Background(), addr); err == nil {
		t.Fatal("ListenAndServe on a taken address = nil error")
	}
}

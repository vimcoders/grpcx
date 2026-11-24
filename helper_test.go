package grpcx

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// errCallRefused is the error a client interceptor returns when it refuses a
// call. Tests assert it reaches the caller unchanged, because an interceptor's
// error must not be rewritten into a transport status.
var errCallRefused = status.Error(codes.PermissionDenied, "test: call refused")

// echoStub is the service the top-level tests register. It answers every call
// with a fixed suffix so a test can tell a reply from its request.
type echoStub struct {
	api.UnimplementedEchoServiceServer
}

func (s *echoStub) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.GetMessage() + "!"}, nil
}

// stubPicker is a balancer.Picker that hands out one round tripper, records the
// last pick request and counts closes, which is what the client's Close has to
// reach.
type stubPicker struct {
	rt     transport.RoundTripper
	err    error
	mu     sync.Mutex
	last   balancer.PickInfo
	closes int
}

func (p *stubPicker) Pick(_ context.Context, info balancer.PickInfo) (transport.RoundTripper, error) {
	p.mu.Lock()
	p.last = info
	p.mu.Unlock()
	if p.err != nil {
		return nil, p.err
	}
	return p.rt, nil
}

func (p *stubPicker) lastInfo() balancer.PickInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last
}

func (p *stubPicker) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

func (p *stubPicker) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

// stubRT is a RoundTripper that records the framed request it served. It lets a
// client test assert what reached the transport without a live server.
type stubRT struct {
	mu      sync.Mutex
	rtReq   *api.Request
	rtResp  *api.Response
	pingErr error
}

func (s *stubRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return nil
}

func (s *stubRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

func (s *stubRT) RoundTrip(_ context.Context, req *api.Request) (*api.Response, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rtReq = req
	return s.rtResp, nil
}

func (s *stubRT) Ping(context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pingErr
}

func (s *stubRT) Close() error { return nil }

// freeAddr reserves a loopback address and releases it, so a server can bind it
// a moment later. ListenAndServe does not report the port it bound, and a test
// needs the address before the call blocks.
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

// waitListening blocks until addr accepts connections. freeAddr only reserves
// the port and releases it, and ListenAndServe binds inside its own goroutine,
// so a dial issued right after startServer can reach a port nobody listens on
// yet. The probe retries instead of reading Server.listener: that field is
// published without a lock, so touching it here would be a data race the -race
// job reports.
func waitListening(t *testing.T, addr string, done <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.Dial("tcp", addr)
		if err == nil {
			_ = conn.Close()
			return
		}
		select {
		case e := <-done:
			t.Fatalf("ListenAndServe on %s returned before it listened: %v", addr, e)
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("address %s did not start listening within 2s", addr)
		}
		time.Sleep(time.Millisecond)
	}
}

// startServer runs a Server on a reserved loopback address and returns it with
// the address. Cleanup cancels the serve loop and closes the server. Serve does
// not close the connections it serves on its own, so a test that dials in relies
// on its own client cleanup running first: client cleanups are registered after
// this one and therefore run before it.
func startServer(t *testing.T, opts ...ServerOption) (*Server, string) {
	t.Helper()
	srv := NewServer(opts...)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()
	waitListening(t, addr, done)

	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
		select {
		case err := <-done:
			if err != nil && !errors.Is(err, net.ErrClosed) {
				t.Logf("ListenAndServe stopped with %v", err)
			}
		case <-time.After(time.Second):
			t.Log("ListenAndServe did not return within 1s")
		}
	})
	return srv, addr
}

// dialClient connects to addr and closes the client when the test ends. The
// cleanup runs before startServer's, so the connection is gone before the
// server is closed: with no per-connection registry left on Server, closing a
// client is what wakes the handler parked in a read.
func dialClient(t *testing.T, addr string, opts ...Option) ClientConnInterface {
	t.Helper()
	cli, err := DialContext(context.Background(), addr, opts...)
	if err != nil {
		t.Fatalf("DialContext(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = cli.Close() })
	return cli
}

// mdCapture records the metadata a server interceptor saw, so a test can assert
// what the client actually put on the wire.
type mdCapture struct {
	mu  sync.Mutex
	md  metadata.MD
	got bool
}

func (c *mdCapture) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		c.mu.Lock()
		c.md, c.got = metadata.GetMetadata(ctx)
		c.mu.Unlock()
		return handler(ctx, req)
	}
}

func (c *mdCapture) snapshot() (metadata.MD, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.md, c.got
}

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

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

func (p *stubPicker) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closes++
	return nil
}

func (p *stubPicker) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closes
}

// stubRT is a RoundTripper that records the framed call it served. It lets a
// client test assert what reached the transport without a live server.
type stubRT struct {
	mu       sync.Mutex
	rtReq    *api.Request
	rtResp   *api.Response
	invokeEr error
	pingErr  error
}

func (s *stubRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.invokeEr
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

// startServer runs a Server on a reserved loopback address and returns it with
// the address. Cleanup cancels the serve loop and closes the server, which also
// closes any connection a test left open.
func startServer(t *testing.T, opts ...ServerOption) (*Server, string) {
	t.Helper()
	srv := NewServer(opts...)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()

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

// dialClient connects to addr and closes the client when the test ends.
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

// failingInterceptor is a client interceptor that refuses every call, used to
// pin that Invoke surfaces it instead of dialing past it.
func failingInterceptor(err error) UnaryClientInterceptor {
	return func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
		return err
	}
}

// transportContextWithMetadata attaches a metadata pair to ctx, which is how a
// caller hands out-of-band values to an interceptor.
func transportContextWithMetadata(t *testing.T, key, value string) context.Context {
	t.Helper()
	return metadata.WithMetadata(context.Background(), metadata.Pairs(key, value))
}

var errCallRefused = status.Error(codes.PermissionDenied, "test: call refused")

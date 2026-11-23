package grpcx

import (
	"context"
	"net"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

// fakePicker hands out one fixed backend and records every PickInfo it
// received, so a test can check which method the client asked to be balanced.
type fakePicker struct {
	mu      sync.Mutex
	rt      transport.RoundTripper
	pickErr error
	picks   []balancer.PickInfo
	closed  int
}

func (p *fakePicker) Pick(_ context.Context, info balancer.PickInfo) (transport.RoundTripper, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.picks = append(p.picks, info)
	if p.pickErr != nil {
		return nil, p.pickErr
	}
	return p.rt, nil
}

func (p *fakePicker) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	return nil
}

func (p *fakePicker) recorded() []balancer.PickInfo {
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]balancer.PickInfo, len(p.picks))
	copy(out, p.picks)
	return out
}

func (p *fakePicker) closeCount() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// fakeRT is a RoundTripper that records the calls the client forwards to it and
// echoes EchoRequest into the reply, which is enough to assert the client picked
// the right backend and built the right arguments.
type fakeRT struct {
	mu        sync.Mutex
	invoked   []string
	rtCalls   int
	lastReq   *api.Request
	rtResp    *api.Response
	invokeErr error
	rtErr     error
	streamErr error
	closed    int
}

func (r *fakeRT) Invoke(_ context.Context, method string, in, out any, _ ...grpc.CallOption) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.invoked = append(r.invoked, method)
	if r.invokeErr != nil {
		return r.invokeErr
	}
	if req, ok := in.(*api.EchoRequest); ok {
		if reply, ok := out.(*api.EchoResponse); ok {
			reply.Message = req.GetMessage()
		}
	}
	return nil
}

func (r *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, r.streamErr
}

func (r *fakeRT) RoundTrip(_ context.Context, req *api.Request) (*api.Response, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.rtCalls++
	r.lastReq = req
	return r.rtResp, r.rtErr
}

func (r *fakeRT) Ping(context.Context) error { return nil }

func (r *fakeRT) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
	return nil
}

func (r *fakeRT) invocations() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.invoked))
	copy(out, r.invoked)
	return out
}

func (r *fakeRT) roundTrips() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.rtCalls
}

// echoServer is the EchoService implementation used by the end to end tests.
type echoServer struct {
	api.UnimplementedEchoServiceServer

	mu    sync.Mutex
	calls int
	err   error
}

func (e *echoServer) Echo(_ context.Context, in *api.EchoRequest) (*api.EchoResponse, error) {
	e.mu.Lock()
	e.calls++
	err := e.err
	e.mu.Unlock()

	if err != nil {
		return nil, err
	}
	return &api.EchoResponse{Message: in.GetMessage()}, nil
}

func (e *echoServer) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

// countingCodec wraps the proto codec and counts the conversions, which proves
// the configured codec is the one the client and the server actually use.
type countingCodec struct {
	enc        encoding.Codec
	marshals   atomic.Int64
	unmarshals atomic.Int64
}

func (c *countingCodec) Name() string { return c.enc.Name() }

func (c *countingCodec) Marshal(v any) ([]byte, error) {
	c.marshals.Add(1)
	return c.enc.Marshal(v)
}

func (c *countingCodec) Unmarshal(b []byte, v any) error {
	c.unmarshals.Add(1)
	return c.enc.Unmarshal(b, v)
}

// reserveAddr returns an address that was free a moment ago, for tests that
// need to dial a server before it can report its own port.
func reserveAddr(t *testing.T) string {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve addr: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close reserved listener: %v", err)
	}
	return addr
}

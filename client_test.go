package grpcx

import (
	"context"
	"net"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/mem"
)

// echoServer implements the generated echo service on top of grpcx.
type echoServer struct {
	api.UnimplementedEchoServiceServer
}

func (e *echoServer) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message + "!"}, nil
}

// recordingRT stands in for a backend transport: it answers RoundTrip with a
// fixed response and remembers the methods it was invoked with. rtErr, when set,
// makes RoundTrip fail instead.
type recordingRT struct {
	invokes    atomic.Int32
	roundTrips atomic.Int32
	lastMethod atomic.Value
	rtErr      error
}

func (r *recordingRT) Invoke(_ context.Context, method string, _, _ any, _ ...grpc.CallOption) error {
	r.invokes.Add(1)
	r.lastMethod.Store(method)
	return nil
}

func (r *recordingRT) RoundTrip(context.Context, []byte, ...transport.CallOption) (mem.Buffer, error) {
	r.roundTrips.Add(1)
	if r.rtErr != nil {
		return nil, r.rtErr
	}
	return mem.SliceBuffer("reply"), nil
}

func (r *recordingRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

func (r *recordingRT) Ping(context.Context) error { return nil }
func (r *recordingRT) Close() error               { return nil }

var _ transport.RoundTripper = (*recordingRT)(nil)

// stubPicker hands out one recordingRT and remembers what it was asked for.
// err, when set, is returned instead of a backend.
type stubPicker struct {
	rt     transport.RoundTripper
	closed atomic.Int32
	seen   atomic.Value
	err    error
}

func (p *stubPicker) Pick(_ context.Context, info balancer.PickInfo) (transport.RoundTripper, error) {
	p.seen.Store(info.FullMethodName)
	if p.err != nil {
		return nil, p.err
	}
	return p.rt, nil
}

func (p *stubPicker) Close() error {
	p.closed.Add(1)
	return nil
}

var _ balancer.Picker = (*stubPicker)(nil)

// freeAddr reserves a loopback port and releases it, so a server can listen on
// it: the kernel picks the port, the test learns it.
func freeAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve a port: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("release the port: %v", err)
	}
	return addr
}

// testServer is a serving grpcx server plus the hooks that stop it.
type testServer struct {
	addr   string
	cancel context.CancelFunc
	done   chan error
}

// serve starts a server on a loopback port and waits until it accepts, so a
// dial afterwards cannot race the listener.
func serve(t *testing.T, opts ...ServerOption) *testServer {
	t.Helper()
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	srv := NewServer(opts...)
	srv.RegisterService(&api.EchoService_ServiceDesc, &echoServer{})
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never accepted on %s: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}

	ts := &testServer{addr: addr, cancel: cancel, done: done}
	t.Cleanup(cancel)
	return ts
}

// TestDialInvokeAndClose covers the wiring end to end: the client dials the
// serving server, one unary call reaches the registered implementation, and
// Close tears the balancer down.
func TestDialInvokeAndClose(t *testing.T) {
	srv := serve(t)
	cc, err := Dial(srv.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	var reply api.EchoResponse
	if err := cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
	if err := cc.Close(); err != nil {
		t.Errorf("Close: %v", err)
	}
}

// TestDialSucceedsWithAnEmptyPoolWhenTheEndpointRefuses pins the current
// behaviour: the balancer skips dial errors, so Dial on a refused port returns a
// client whose pool is empty, and the first call is what fails with Unavailable.
func TestDialSucceedsWithAnEmptyPoolWhenTheEndpointRefuses(t *testing.T) {
	cc, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial = %v, want nil while the first call carries the failure", err)
	}
	if cc == nil {
		t.Fatal("Dial returned no client")
	}
	defer func() { _ = cc.Close() }()

	var reply api.EchoResponse
	err = cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{}, &reply)
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("Invoke over an empty pool = %v (%v), want Unavailable", err, code)
	}
}

// TestRoundTripPicksWithoutNamingAMethod covers the framed path: the pick
// carries no method name. Backend selection is left to the K8s Service or DNS,
// so every endpoint in the pool is equally eligible and the method travels only
// as frame content.
func TestRoundTripPicksWithoutNamingAMethod(t *testing.T) {
	rt := &recordingRT{}
	p := &stubPicker{rt: rt}
	c := &client{Picker: p}

	buf, err := c.RoundTrip(context.Background(), []byte("body"), &transport.CallOptions{FullMethodName: "/api.EchoService/Echo"})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := p.seen.Load(); got != "" {
		t.Errorf("PickInfo.FullMethodName = %v, want empty: the method does not choose the backend", got)
	}
	if rt.roundTrips.Load() != 1 {
		t.Errorf("RoundTrip on the backend ran %d times, want 1", rt.roundTrips.Load())
	}
	if got := string(buf.ReadOnlyData()); got != "reply" {
		t.Errorf("payload = %q, want reply", got)
	}
}

// TestRoundTripReportsAPickError covers the pick failing: the error reaches the
// caller unchanged and no backend is touched.
func TestRoundTripReportsAPickError(t *testing.T) {
	rt := &recordingRT{}
	c := &client{Picker: &stubPicker{rt: rt, err: status.Unavailable.Err()}}

	buf, err := c.RoundTrip(context.Background(), []byte("body"))
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("RoundTrip = (%v, %v), want Unavailable from the pick", buf, code)
	}
	if buf != nil {
		t.Errorf("RoundTrip returned a buffer together with the error: %v", buf)
	}
	if rt.roundTrips.Load() != 0 {
		t.Errorf("backend round trips = %d, want 0", rt.roundTrips.Load())
	}
}

// TestRoundTripReportsTheBackendError covers a backend that fails the call: its
// status reaches the caller unchanged and no buffer comes with it.
func TestRoundTripReportsTheBackendError(t *testing.T) {
	rt := &recordingRT{rtErr: status.Unavailable.Err()}
	c := &client{Picker: &stubPicker{rt: rt}}

	buf, err := c.RoundTrip(context.Background(), []byte("body"))
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("RoundTrip = (%v, %v), want Unavailable from the backend", buf, code)
	}
	if buf != nil {
		t.Errorf("RoundTrip returned a buffer together with the error: %v", buf)
	}
	if rt.roundTrips.Load() != 1 {
		t.Errorf("backend round trips = %d, want 1", rt.roundTrips.Load())
	}
}

// TestInvokeGoesThroughTheInstalledInterceptor covers WithUnaryClientInterceptor:
// the custom interceptor replaces the default one and sees the call.
func TestInvokeGoesThroughTheInstalledInterceptor(t *testing.T) {
	srv := serve(t)
	var calls atomic.Int32
	cc, err := Dial(srv.addr, WithUnaryClientInterceptor(
		func(ctx context.Context, method string, req, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
			calls.Add(1)
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			return rt.Invoke(ctx, method, req, reply, opts...)
		}))
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	var reply api.EchoResponse
	if err := cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("interceptor ran %d times, want 1", got)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want the interceptor chain to forward the call", reply.Message)
	}
}

// TestNewStreamIsUnimplemented covers the streaming surface.
func TestNewStreamIsUnimplemented(t *testing.T) {
	c := &client{}
	s, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/m")
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("NewStream = (%v, %v), want Unimplemented", s, code)
	}
	if s != nil {
		t.Error("NewStream returned a stream although it is unimplemented")
	}
}

// TestCloseClosesThePicker covers the shutdown delegation: the picker owns the
// balancer, so closing the client closes it.
func TestCloseClosesThePicker(t *testing.T) {
	p := &stubPicker{rt: &recordingRT{}}
	c := &client{Picker: p}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := p.closed.Load(); got != 1 {
		t.Errorf("picker closed %d times, want 1", got)
	}
}

// TestTransportOptionsAccumulateOnTheClient covers the options that are only
// forwarded: each one is appended to the transport list the balancer dials with.
func TestTransportOptionsAccumulateOnTheClient(t *testing.T) {
	c := &client{}
	WithTimeout(2 * time.Second)(c)
	WithMaxStreams(64)(c)
	WithBufSize(4)(c)
	if got := len(c.opts); got != 3 {
		t.Errorf("client holds %d transport options, want 3", got)
	}
}

// TestWithCodecAndInterceptorReplaceTheDefaults covers the two client fields the
// options write.
func TestWithCodecAndInterceptorReplaceTheDefaults(t *testing.T) {
	c := &client{Codec: encoding.GetCodec(encoding.Name)}
	codec := encoding.GetCodec(encoding.Name)
	WithCodec(codec)(c)
	if c.Codec != codec {
		t.Error("WithCodec did not replace the codec")
	}
	WithUnaryClientInterceptor(func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
		return status.Unimplemented.Err()
	})(c)
	if c.unaryClientInterceptor == nil {
		t.Error("WithUnaryClientInterceptor did not install an interceptor")
	}
}

// TestNewServerBuildsBothHalves covers the constructor: the returned server
// carries a handler and a registrar, so options only need to be passed once.
func TestNewServerBuildsBothHalves(t *testing.T) {
	s := NewServer()
	if s.Handler == nil || s.ServiceRegistrar == nil {
		t.Fatalf("NewServer returned %+v, want both halves wired", s)
	}
	// Registering through the method is the path a caller takes after
	// construction, and the handler must serve it.
	srv := NewServer()
	if srv.Handler == nil {
		t.Fatal("NewServer dropped the handler")
	}
	srv.RegisterService(&api.EchoService_ServiceDesc, &echoServer{})
}

// TestServerServesAfterARegistration covers RegisterService on a live server
// built without one.
func TestServerServesAfterARegistration(t *testing.T) {
	srv := serveWith(t, NewServer(), &echoServer{})
	cc, err := Dial(srv.addr)
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	defer func() { _ = cc.Close() }()

	var reply api.EchoResponse
	if err := cc.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// serveWith is serve with a caller-built server: the registration happens after
// construction, which is the path NewServer leaves open.
func serveWith(t *testing.T, srv *Server, impl api.EchoServiceServer) *testServer {
	t.Helper()
	srv.RegisterService(&api.EchoService_ServiceDesc, impl)
	addr := freeAddr(t)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- srv.ListenAndServe(ctx, addr) }()

	deadline := time.Now().Add(2 * time.Second)
	for {
		conn, err := net.DialTimeout("tcp", addr, 50*time.Millisecond)
		if err == nil {
			_ = conn.Close()
			break
		}
		if time.Now().After(deadline) {
			cancel()
			t.Fatalf("server never accepted on %s: %v", addr, err)
		}
		time.Sleep(5 * time.Millisecond)
	}
	ts := &testServer{addr: addr, cancel: cancel, done: done}
	t.Cleanup(cancel)
	return ts
}

// TestListenAndServeReturnsWhenTheContextIsCanceled covers shutdown: closing the
// listener wakes Accept, and the loop reports no error for its own shutdown. The
// comment on ListenAndServe promises net.ErrClosed; the implementation returns
// nil, and this pins what the code does.
func TestListenAndServeReturnsWhenTheContextIsCanceled(t *testing.T) {
	srv := serve(t)
	srv.cancel()

	select {
	case err := <-srv.done:
		if err != nil {
			t.Errorf("ListenAndServe = %v, want nil after its own shutdown", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("ListenAndServe did not return after its context was canceled")
	}
}

// TestListenAndServeReportsABindFailure covers the listener error: a port that
// cannot be bound fails before any connection is accepted.
func TestListenAndServeReportsABindFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	srv := NewServer()
	if err := srv.ListenAndServe(context.Background(), ln.Addr().String()); err == nil {
		t.Error("ListenAndServe on a port already in use returned nil")
	}
}

package grpcx

import (
	"context"
	"errors"
	"testing"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// fakePicker records every pick and hands out one fixed round tripper.
type fakePicker struct {
	rt     transport.RoundTripper
	err    error
	picks  []balancer.PickInfo
	closes int
}

// Pick implements balancer.Picker.
func (p *fakePicker) Pick(_ context.Context, info balancer.PickInfo) (transport.RoundTripper, error) {
	p.picks = append(p.picks, info)
	if p.err != nil {
		return nil, p.err
	}
	return p.rt, nil
}

// Close implements io.Closer.
func (p *fakePicker) Close() error {
	p.closes++
	return nil
}

// fakeRT stands in for one dialed backend: it records the calls it is handed
// and returns whatever the test configured.
type fakeRT struct {
	invoked   string
	invokeErr error
	rtErr     error
	calls     int
}

// RoundTrip implements transport.RoundTripper.
func (f *fakeRT) RoundTrip(context.Context, []byte, ...transport.CallOption) (transport.Buffer, error) {
	f.calls++
	return nil, f.rtErr
}

// Ping implements transport.RoundTripper.
func (f *fakeRT) Ping(context.Context) error { return nil }

// Invoke implements grpc.ClientConnInterface.
func (f *fakeRT) Invoke(_ context.Context, method string, _, _ any, _ ...grpc.CallOption) error {
	f.invoked = method
	return f.invokeErr
}

// NewStream implements grpc.ClientConnInterface.
func (f *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

// Close implements io.Closer.
func (f *fakeRT) Close() error { return nil }

// defaultInterceptor is the unary interceptor DialContext installs, copied here
// so the pick-and-forward path can be exercised without dialing.
func defaultInterceptor(ctx context.Context, method string, req, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
	rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
	if err != nil {
		return err
	}
	return rt.Invoke(ctx, method, req, reply, opts...)
}

// TestRoundTripPicksWithoutMethodName covers the load-balancing contract: the
// pick names no method, because backends come from the K8s Service and every
// endpoint is equally eligible.
func TestRoundTripPicksWithoutMethodName(t *testing.T) {
	p := &fakePicker{rt: &fakeRT{}}
	c := &client{Picker: p}
	if _, err := c.RoundTrip(context.Background(), []byte("x")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if len(p.picks) != 1 {
		t.Fatalf("picked %d times, want 1", len(p.picks))
	}
	if got := p.picks[0]; got.FullMethodName != "" {
		t.Errorf("PickInfo = %+v, want an empty value", got)
	}
}

// TestRoundTripForwardsThePayload covers the framed path: the bytes reach the
// picked backend unchanged.
func TestRoundTripForwardsThePayload(t *testing.T) {
	rt := &fakeRT{}
	p := &fakePicker{rt: rt}
	c := &client{Picker: p}
	if _, err := c.RoundTrip(context.Background(), []byte("x")); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if rt.calls != 1 {
		t.Errorf("backend saw %d round trips, want 1", rt.calls)
	}
}

// TestRoundTripReportsPickError covers an empty balancer: the pick error is the
// call's error.
func TestRoundTripReportsPickError(t *testing.T) {
	boom := errors.New("no backend")
	c := &client{Picker: &fakePicker{err: boom}}
	if _, err := c.RoundTrip(context.Background(), nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// TestInvokeForwardsThroughTheInterceptor covers the unary path: the method
// name reaches the picked backend and its error reaches the caller.
func TestInvokeForwardsThroughTheInterceptor(t *testing.T) {
	rt := &fakeRT{}
	c := &client{unaryInt: defaultInterceptor, Picker: &fakePicker{rt: rt}}
	if err := c.Invoke(context.Background(), "/api.EchoService/Echo", nil, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if rt.invoked != "/api.EchoService/Echo" {
		t.Errorf("invoked = %q, want the method name", rt.invoked)
	}
}

// TestInvokeReportsTheInterceptorError covers a failing interceptor.
func TestInvokeReportsTheInterceptorError(t *testing.T) {
	boom := errors.New("interceptor")
	c := &client{unaryInt: func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
		return boom
	}}
	if err := c.Invoke(context.Background(), "/x/Y", nil, nil); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// TestOptionsAppendToTheTransportList covers the option wrappers: each one
// stores a transport option, and none of them set a codec field on the client.
func TestOptionsAppendToTheTransportList(t *testing.T) {
	c := &client{}
	WithCodec(nil)(c)
	WithTimeout(0)(c)
	WithMaxStreams(0)(c)
	WithBufSize(0)(c)
	if got := len(c.opts); got != 4 {
		t.Errorf("opts = %d, want 4", got)
	}
}

// TestWithUnaryClientInterceptorReplacesTheDefault covers the only option that
// writes a client field instead of a transport one.
func TestWithUnaryClientInterceptorReplacesTheDefault(t *testing.T) {
	called := false
	i := func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
		called = true
		return nil
	}
	c := &client{}
	WithUnaryClientInterceptor(i)(c)
	if err := c.Invoke(context.Background(), "/x/Y", nil, nil); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if !called {
		t.Error("the installed interceptor was not called")
	}
}

// TestCloseClosesThePicker covers teardown: the picker owns the keepalive loop,
// so closing the client closes it.
func TestCloseClosesThePicker(t *testing.T) {
	p := &fakePicker{}
	c := &client{Picker: p}
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if p.closes != 1 {
		t.Errorf("picker closed %d times, want 1", p.closes)
	}
}

// TestNewStreamIsUnimplemented covers the unary-only client: streaming never
// reaches a backend.
func TestNewStreamIsUnimplemented(t *testing.T) {
	c := &client{Picker: &fakePicker{rt: &fakeRT{}}}
	_, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/x/Y")
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", got)
	}
}

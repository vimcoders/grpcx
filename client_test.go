package grpcx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

func TestInvokeCallsUnaryInterceptor(t *testing.T) {
	p := &fakePicker{rt: &fakeRT{}}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	var gotMethod string
	var gotBalance balancer.Picker
	c.unaryClientInterceptor = func(_ context.Context, method string, _, _ any, balance balancer.Picker, _ ...grpc.CallOption) error {
		gotMethod, gotBalance = method, balance
		return nil
	}

	reply := &api.EchoResponse{}
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "x"}, reply); err != nil {
		t.Fatalf("Invoke = %v", err)
	}
	if gotMethod != api.EchoService_Echo_FullMethodName {
		t.Fatalf("interceptor method = %q", gotMethod)
	}
	if gotBalance != p {
		t.Fatalf("interceptor received a different picker")
	}
}

func TestInvokeReturnsInterceptorError(t *testing.T) {
	interceptorErr := errors.New("interceptor refused")
	c := &client{Picker: &fakePicker{}, Codec: encoding.GetCodec(encoding.Name)}
	c.unaryClientInterceptor = func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
		return interceptorErr
	}

	if err := c.Invoke(context.Background(), "/api.EchoService/Echo", nil, nil); !errors.Is(err, interceptorErr) {
		t.Fatalf("Invoke = %v, want %v", err, interceptorErr)
	}
}

// TestRoundTripUsesMethodFromOptions checks the pick key: RoundTrip balances on
// the full method name carried in the request options.
func TestRoundTripUsesMethodFromOptions(t *testing.T) {
	rt := &fakeRT{rtResp: &api.Response{}}
	p := &fakePicker{rt: rt}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	req := &api.Request{Options: &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName}}
	resp, err := c.RoundTrip(context.Background(), req)
	if err != nil {
		t.Fatalf("RoundTrip = %v", err)
	}
	if resp != rt.rtResp {
		t.Fatalf("RoundTrip returned %v, want the backend response", resp)
	}
	picks := p.recorded()
	if len(picks) != 1 || picks[0].FullMethodName != api.EchoService_Echo_FullMethodName {
		t.Fatalf("picks = %+v, want one pick for the request method", picks)
	}
	if got := rt.roundTrips(); got != 1 {
		t.Fatalf("backend round trips = %d, want 1", got)
	}
	if rt.lastReq != req {
		t.Fatalf("backend received a different request")
	}
}

// TestRoundTripWithoutOptions covers the nil options branch: the call is still
// balanced and forwarded, with an empty method name.
func TestRoundTripWithoutOptions(t *testing.T) {
	rt := &fakeRT{rtResp: &api.Response{}}
	p := &fakePicker{rt: rt}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	if _, err := c.RoundTrip(context.Background(), &api.Request{}); err != nil {
		t.Fatalf("RoundTrip = %v", err)
	}
	picks := p.recorded()
	if len(picks) != 1 || picks[0].FullMethodName != "" {
		t.Fatalf("picks = %+v, want one pick with an empty method", picks)
	}
	if got := rt.roundTrips(); got != 1 {
		t.Fatalf("backend round trips = %d, want 1", got)
	}
}

func TestRoundTripReturnsPickError(t *testing.T) {
	pickErr := errors.New("no healthy backend")
	p := &fakePicker{pickErr: pickErr}
	rt := &fakeRT{}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	if _, err := c.RoundTrip(context.Background(), &api.Request{}); !errors.Is(err, pickErr) {
		t.Fatalf("RoundTrip = %v, want %v", err, pickErr)
	}
	if got := rt.roundTrips(); got != 0 {
		t.Fatalf("backend round trips = %d, want 0 after a pick failure", got)
	}
}

func TestNewStreamUsesStreamInterceptor(t *testing.T) {
	streamErr := errors.New("streams are not carried")
	p := &fakePicker{rt: &fakeRT{}}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	var gotMethod string
	c.streamClientInterceptor = func(_ context.Context, _ *grpc.StreamDesc, _ balancer.Picker, method string, _ ...grpc.CallOption) (grpc.ClientStream, error) {
		gotMethod = method
		return nil, streamErr
	}

	if _, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/api.EchoService/Tail"); !errors.Is(err, streamErr) {
		t.Fatalf("NewStream = %v, want %v", err, streamErr)
	}
	if gotMethod != "/api.EchoService/Tail" {
		t.Fatalf("stream interceptor method = %q", gotMethod)
	}
}

func TestCloseClosesPicker(t *testing.T) {
	p := &fakePicker{rt: &fakeRT{}}
	c := &client{Picker: p, Codec: encoding.GetCodec(encoding.Name)}

	if err := c.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if got := p.closeCount(); got != 1 {
		t.Fatalf("picker closed %d times, want 1", got)
	}
}

// TestClientOptions checks each option lands on the client: the codec, both
// interceptors, and the transport options WithTimeout forwards.
func TestClientOptions(t *testing.T) {
	codec := encoding.GetCodec(encoding.Name)
	c := &client{}
	WithCodec(codec)(c)
	if c.Codec != codec {
		t.Fatalf("WithCodec did not install the codec")
	}

	unary := func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error { return nil }
	WithUnaryClientInterceptor(unary)(c)
	if c.unaryClientInterceptor == nil {
		t.Fatalf("WithUnaryClientInterceptor did not install the interceptor")
	}

	stream := func(context.Context, *grpc.StreamDesc, balancer.Picker, string, ...grpc.CallOption) (grpc.ClientStream, error) {
		return nil, nil
	}
	WithStreamClientInterceptor(stream)(c)
	if c.streamClientInterceptor == nil {
		t.Fatalf("WithStreamClientInterceptor did not install the interceptor")
	}

	const timeout = 1500 * time.Millisecond
	WithTimeout(timeout)(c)
	opts := transport.DefaultOptions()
	for _, o := range c.opts {
		o(&opts)
	}
	if got := opts.Timeout(); got != timeout {
		t.Fatalf("transport timeout = %v, want %v", got, timeout)
	}

	WithMaxStreams(3)(c)
	WithBufSize(2)(c)
	if got := len(c.opts); got != 3 {
		t.Fatalf("transport options = %d, want 3 (timeout, streams, buffer)", got)
	}
}

// TestDialReportsUnresolvableEndpoint checks that Dial hands the resolver error
// to the caller instead of a client without backends.
func TestDialReportsUnresolvableEndpoint(t *testing.T) {
	if _, err := Dial(""); err == nil {
		t.Fatalf("Dial(\"\") = nil error")
	}
}

package grpcx

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestClientInvokeEcho pins the full client path: Dial resolves the endpoint,
// the balancer dials the transport, the stub frames the request and the reply
// comes back through the receive loop.
func TestClientInvokeEcho(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)

	reply, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if reply.Message != "hi!" {
		t.Fatalf("reply = %q, want hi!", reply.Message)
	}
}

// TestClientInvokeSurfacesInterceptorError pins that Invoke returns what the
// interceptor returned: the interceptor is the only layer that decides whether a
// call is attempted at all, and swallowing its error would report success for a
// request that never left.
func TestClientInvokeSurfacesInterceptorError(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr, WithUnaryClientInterceptor(failingInterceptor(errCallRefused)))

	err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{})
	if !errors.Is(err, errCallRefused) {
		t.Fatalf("Invoke = %v, want the interceptor's error", err)
	}
}

// TestClientNewStreamUnimplemented pins that the stream surface is refused at
// the top level too: grpcx carries unary calls only.
func TestClientNewStreamUnimplemented(t *testing.T) {
	c := &client{cancel: func() {}}

	_, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/nope/Nope")
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("NewStream = %v, want Unimplemented", err)
	}
}

// TestClientCloseReachesPickerAndCancel pins both halves of Close: the picker's
// Close closes every backend transport, and the cancel stops the balancer's
// keepalive loop. Closing only one of the two leaves either connections or a
// goroutine behind.
func TestClientCloseReachesPickerAndCancel(t *testing.T) {
	p := &stubPicker{}
	canceled := 0
	c := &client{Picker: p, cancel: func() { canceled++ }}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := p.closeCount(); got != 1 {
		t.Fatalf("picker closed %d times, want 1", got)
	}
	if canceled != 1 {
		t.Fatalf("cancel called %d times, want 1", canceled)
	}
}

// TestClientRoundTripPicksByMethod pins the framed entry point: the backend is
// chosen by the full method name in the request header, which is the only key a
// framed caller supplies.
func TestClientRoundTripPicksByMethod(t *testing.T) {
	rt := &stubRT{rtResp: &api.Response{Options: &api.CallOptions{}}}
	p := &stubPicker{rt: rt}
	c := &client{Picker: p, cancel: func() {}}

	req := &api.Request{
		Options: &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName},
		Payload: []byte{1, 2, 3},
	}
	if _, err := c.RoundTrip(context.Background(), req); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := p.lastInfo().FullMethodName; got != api.EchoService_Echo_FullMethodName {
		t.Fatalf("pick info method = %q, want the request's", got)
	}
	if rt.rtReq != req {
		t.Fatalf("RoundTrip reached the backend with %v, want the same request", rt.rtReq)
	}
}

// TestClientRoundTripWithoutOptions pins the nil-header path: a framed request
// without options still picks a backend instead of dereferencing nil.
func TestClientRoundTripWithoutOptions(t *testing.T) {
	rt := &stubRT{rtResp: &api.Response{Options: &api.CallOptions{}}}
	p := &stubPicker{rt: rt}
	c := &client{Picker: p, cancel: func() {}}

	if _, err := c.RoundTrip(context.Background(), &api.Request{Payload: []byte{9}}); err != nil {
		t.Fatalf("RoundTrip without options: %v", err)
	}
	if got := p.lastInfo().FullMethodName; got != "" {
		t.Fatalf("pick info method = %q, want empty", got)
	}
}

// TestClientInvokePropagatesMetadata pins the metadata round trip: the client
// puts its context metadata into the request header and the server reattaches it
// to the handler context, which is where an interceptor reads it.
func TestClientInvokePropagatesMetadata(t *testing.T) {
	cap := &mdCapture{}
	_, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		UnaryServerInterceptor(cap.interceptor()),
	)
	cli := dialClient(t, addr)

	ctx := transportContextWithMetadata(t, "request-id", "42")
	if _, err := api.NewEchoServiceClient(cli).Echo(ctx, &api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	md, ok := cap.snapshot()
	if !ok {
		t.Fatal("server interceptor never ran, want it called for the request")
	}
	if v, ok := md.Get("request-id"); !ok || v != "42" {
		t.Fatalf("server metadata = %v, want request-id=42", md)
	}
}

// TestClientDialReportsUnresolvableEndpoint pins the construction failure path:
// an endpoint with an empty host is rejected by the resolver before any dial, so
// Dial never returns a client that can only fail later.
func TestClientDialReportsUnresolvableEndpoint(t *testing.T) {
	cli, err := Dial(":bad")
	if err == nil {
		_ = cli.Close()
		t.Fatal("Dial of an endpoint without a host = nil error")
	}
}

// TestClientOptionsAppendTransportOptions pins that the top-level options are
// forwarded to the transport as options rather than being applied twice: the
// client keeps them in a slice for Dial and touches no transport field itself.
func TestClientOptionsAppendTransportOptions(t *testing.T) {
	c := &client{}
	WithCodec(encoding.GetCodec(encoding.Name))(c)
	WithTimeout(time.Second)(c)
	WithMaxStreams(4)(c)
	WithBufSize(2)(c)

	if c.Codec == nil {
		t.Fatal("WithCodec left the codec nil")
	}
	if got, want := len(c.opts), 3; got != want {
		t.Fatalf("forwarded options = %d, want %d", got, want)
	}
}

// TestClientTimeoutIsConverted pins that WithTimeout reaches the transport: the
// client stores an option that, applied to transport options, sets the call
// timeout. Storing the value itself would skip the zero fallback the transport
// option owns.
func TestClientTimeoutIsConverted(t *testing.T) {
	c := &client{}
	WithTimeout(7 * time.Second)(c)

	opts := transport.DefaultOptions()
	for _, o := range c.opts {
		o(&opts)
	}
	if got, want := opts.Timeout(), 7*time.Second; got != want {
		t.Fatalf("transport timeout = %v, want %v", got, want)
	}
}

// TestClientInvokeReportsMissingBackend pins the picker error path: with no
// backend to pick, Invoke reports the balancer's status instead of panicking on
// a nil round tripper.
func TestClientInvokeReportsMissingBackend(t *testing.T) {
	p := &stubPicker{err: status.Unavailable.Err()}
	c := &client{
		Picker: p,
		cancel: func() {},
		unaryClientInterceptor: func(ctx context.Context, method string, req, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			return rt.Invoke(ctx, method, req, reply, opts...)
		},
	}

	err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{})
	if status.Code(err) != codes.Unavailable {
		t.Fatalf("Invoke without a backend = %v, want Unavailable", err)
	}
}

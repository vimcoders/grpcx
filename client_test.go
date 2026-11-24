package grpcx

import (
	"context"
	"errors"
	"testing"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestClientInvokeEcho pins the end-to-end unary path: a client built by
// DialContext reaches a registered service over a real socket and gets the
// reply back through the generated stub.
func TestClientInvokeEcho(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))
	cli := dialClient(t, addr)

	reply, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "client"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := reply.GetMessage(); got != "client!" {
		t.Fatalf("reply = %q, want client!", got)
	}
}

// TestClientInvokeThroughInterceptor pins that Invoke hands the call to the
// interceptor installed by WithUnaryClientInterceptor, together with the picker
// the connection was built from. An interceptor that never runs is silently
// missing auth or tracing on every call.
func TestClientInvokeThroughInterceptor(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))

	var (
		calls    int
		gotPickr balancer.Picker
	)
	cli, err := DialContext(context.Background(), addr, WithUnaryClientInterceptor(
		func(ctx context.Context, method string, req, reply any, picker balancer.Picker, opts ...grpc.CallOption) error {
			calls++
			gotPickr = picker
			rt, err := picker.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			return rt.Invoke(ctx, method, req, reply, opts...)
		},
	))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	reply, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "intercepted"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if got := reply.GetMessage(); got != "intercepted!" {
		t.Fatalf("reply = %q, want intercepted!", got)
	}
	if calls != 1 {
		t.Fatalf("interceptor ran %d times, want 1", calls)
	}
	if gotPickr == nil {
		t.Fatal("interceptor received a nil picker")
	}
}

// TestClientInvokePropagatesInterceptorError pins that an interceptor error
// reaches the caller unchanged: it must not be rewritten into a transport
// status, or a policy denial would look like a connection problem.
func TestClientInvokePropagatesInterceptorError(t *testing.T) {
	_, addr := startServer(t, transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}))

	cli, err := DialContext(context.Background(), addr, WithUnaryClientInterceptor(
		func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
			return errCallRefused
		},
	))
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	t.Cleanup(func() { _ = cli.Close() })

	var reply api.EchoResponse
	err = cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "x"}, &reply)
	if !errors.Is(err, errCallRefused) {
		t.Fatalf("Invoke = %v, want the interceptor's error", err)
	}
	if reply.GetMessage() != "" {
		t.Fatalf("reply = %q, want it untouched by a refused call", reply.GetMessage())
	}
}

// TestClientInvokeCarriesMetadata pins the out-of-band path: metadata attached
// to the client context reaches the server interceptor, because a caller
// passing a tenant or a trace id relies on it surviving the round trip.
func TestClientInvokeCarriesMetadata(t *testing.T) {
	capture := &mdCapture{}
	_, addr := startServer(t,
		transport.RegisterService(&api.EchoService_ServiceDesc, &echoStub{}),
		UnaryServerInterceptor(capture.interceptor()),
	)
	cli := dialClient(t, addr)

	ctx := metadata.WithMetadata(context.Background(), metadata.Pairs("x-tenant", "acme"))
	if _, err := api.NewEchoServiceClient(cli).Echo(ctx, &api.EchoRequest{Message: "md"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}

	md, ok := capture.snapshot()
	if !ok {
		t.Fatal("server interceptor saw no metadata at all")
	}
	if v, _ := md.Get("x-tenant"); v != "acme" {
		t.Fatalf("x-tenant = %q, want acme", v)
	}
}

// TestClientRoundTripPicksByMethod pins that RoundTrip resolves its backend from
// the full method name in req.Options and forwards the framed request as it
// stands.
func TestClientRoundTripPicksByMethod(t *testing.T) {
	rt := &stubRT{rtResp: &api.Response{Payload: []byte("ok")}}
	picker := &stubPicker{rt: rt}
	c := &client{Picker: picker}

	resp, err := c.RoundTrip(context.Background(), &api.Request{
		Options: &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName},
	})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if got := picker.lastInfo().FullMethodName; got != api.EchoService_Echo_FullMethodName {
		t.Fatalf("picked for %q, want %q", got, api.EchoService_Echo_FullMethodName)
	}
	if rt.rtReq == nil {
		t.Fatal("round tripper received no request")
	}
	if got := string(resp.GetPayload()); got != "ok" {
		t.Fatalf("payload = %q, want ok", got)
	}
}

// TestClientRoundTripAllowsNilOptions pins the tolerant read of req.Options:
// a request built without a header still reaches a backend under an empty
// method name instead of panicking.
func TestClientRoundTripAllowsNilOptions(t *testing.T) {
	picker := &stubPicker{rt: &stubRT{rtResp: &api.Response{}}}
	c := &client{Picker: picker}

	if _, err := c.RoundTrip(context.Background(), &api.Request{}); err != nil {
		t.Fatalf("RoundTrip with nil options: %v", err)
	}
	if got := picker.lastInfo().FullMethodName; got != "" {
		t.Fatalf("picked for %q, want the empty method name", got)
	}
}

// TestClientRoundTripPickerError pins that a picker with no usable backend
// fails the call instead of handing a nil round tripper to the transport.
func TestClientRoundTripPickerError(t *testing.T) {
	c := &client{Picker: &stubPicker{err: status.Unavailable.Err()}}

	_, err := c.RoundTrip(context.Background(), &api.Request{Options: &api.CallOptions{FullMethodName: "/x"}})
	if err == nil {
		t.Fatal("RoundTrip with a failing picker = nil error")
	}
	if got := status.Code(err); got != codes.Unavailable {
		t.Fatalf("code = %v, want Unavailable", got)
	}
}

// TestClientNewStreamIsUnimplemented pins that streaming is refused rather than
// silently half-implemented: grpcx carries unary calls only, and there is no
// stream interceptor left to hand the call to.
func TestClientNewStreamIsUnimplemented(t *testing.T) {
	c := &client{}

	stream, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, api.EchoService_Echo_FullMethodName)
	if stream != nil {
		t.Fatal("NewStream returned a stream for an unsupported call")
	}
	if got := status.Code(err); got != codes.Unimplemented {
		t.Fatalf("code = %v, want Unimplemented", got)
	}
}

// TestClientCloseClosesPickerAndCancelsContext pins both halves of Close: the
// picker is closed, and the child context derived in DialContext is cancelled
// even though the picker no longer holds it. A cancel that never fires leaves
// the keepalive loop running after the client is gone.
func TestClientCloseClosesPickerAndCancelsContext(t *testing.T) {
	picker := &stubPicker{rt: &stubRT{}}
	cancelled := false
	c := &client{
		Picker: picker,
		cancel: func() { cancelled = true },
	}

	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if got := picker.closeCount(); got != 1 {
		t.Fatalf("picker closed %d times, want 1", got)
	}
	if !cancelled {
		t.Fatal("Close did not cancel the child context")
	}
}

// TestClientDialUnreachableEndpointFails pins the startup failure path: a port
// nobody listens on is reported to the caller instead of yielding a client that
// fails every call later.
func TestClientDialUnreachableEndpointFails(t *testing.T) {
	if _, err := DialContext(context.Background(), "127.0.0.1:1"); err == nil {
		t.Fatal("DialContext on a closed port = nil error")
	}
}

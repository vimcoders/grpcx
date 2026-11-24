package grpcx

import (
	"context"
	"testing"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestClientInvokeThroughInterceptor pins that DialContext installs the given
// interceptor and that it sees the resolved full method name.
func TestClientInvokeThroughInterceptor(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	var gotMethod string
	cc, err := DialContext(context.Background(), h.addr, WithUnaryClientInterceptor(
		func(ctx context.Context, method string, req, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
			gotMethod = method
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			return rt.Invoke(ctx, method, req, reply, opts...)
		}))
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if gotMethod != api.EchoService_Echo_FullMethodName {
		t.Fatalf("interceptor saw method %q, want %q", gotMethod, api.EchoService_Echo_FullMethodName)
	}
}

// TestClientInvokePropagatesInterceptorError pins that an interceptor error is
// returned to the caller unchanged, without reaching the server.
func TestClientInvokePropagatesInterceptorError(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))

	refused := status.Error(codes.PermissionDenied, "test: call refused")
	cc, err := DialContext(context.Background(), h.addr, WithUnaryClientInterceptor(
		func(context.Context, string, any, any, balancer.Picker, ...grpc.CallOption) error {
			return refused
		}))
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); status.Code(err) != codes.PermissionDenied {
		t.Fatalf("Echo error code = %v (err = %v), want PermissionDenied", status.Code(err), err)
	}
}

// TestClientRoundTripSendsFramedRequest drives the framed entry point directly:
// the caller marshals the payload and reads the reply out of the response.
func TestClientRoundTripSendsFramedRequest(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	payload, err := encoding.Marshal(&api.EchoRequest{Message: "framed"})
	if err != nil {
		t.Fatalf("marshal request: %v", err)
	}
	resp, err := cc.RoundTrip(context.Background(), &api.Request{
		Options: &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName},
		Payload: payload,
	})
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	var out api.EchoResponse
	if err := encoding.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if out.Message != "framed!" {
		t.Fatalf("RoundTrip reply = %q, want %q", out.Message, "framed!")
	}
}

// TestClientNewStreamIsUnimplemented pins the unary-only contract: streaming
// calls never reach the wire, they are rejected up front.
func TestClientNewStreamIsUnimplemented(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc := dialClient(t, h)

	if _, err := cc.NewStream(context.Background(), &grpc.StreamDesc{}, "/api.EchoService/Stream"); status.Code(err) != codes.Unimplemented {
		t.Fatalf("NewStream code = %v (err = %v), want Unimplemented", status.Code(err), err)
	}
}

// TestClientCloseIsIdempotent pins that Close can run more than once: it only
// cancels the context and returns, so repeated calls report nil and the caller
// does not need a flag of its own.
func TestClientCloseIsIdempotent(t *testing.T) {
	h := startServer(t, withEcho, WithServerReadTimeout(shortRead))
	cc, err := Dial(h.addr)
	if err != nil {
		t.Fatalf("dial %s: %v", h.addr, err)
	}

	for i := range 2 {
		if err := cc.Close(); err != nil {
			t.Fatalf("Close #%d = %v, want nil", i+1, err)
		}
	}
}

// TestClientDialUnreachableReturnsClient pins the tolerant dial contract: a
// resolvable endpoint with nothing listening still yields a client, and the
// failure surfaces on the first call as an empty-pool pick.
func TestClientDialUnreachableReturnsClient(t *testing.T) {
	cc, err := Dial("127.0.0.1:1")
	if err != nil {
		t.Fatalf("Dial on a closed port = %v, want nil", err)
	}
	t.Cleanup(func() { _ = cc.Close() })

	if _, err := echoClient(cc).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("Echo on an empty pool code = %v (err = %v), want ResourceExhausted", status.Code(err), err)
	}
}

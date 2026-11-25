package transport

import (
	"context"
	"encoding/binary"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestHandleServesUnaryCall pins the whole server path over one connection: the
// request frame is dispatched by method name, the registered implementation
// answers, and the reply reaches the caller.
func TestHandleServesUnaryCall(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	reply, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "hi"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if reply.Message != "hi!" {
		t.Fatalf("reply = %q, want %q", reply.Message, "hi!")
	}
}

// TestHandleAnswersUnknownMethod pins the fallback over a real connection: a
// method the descriptor does not expose is answered with Unimplemented carrying
// its name, and the connection stays usable.
func TestHandleAnswersUnknownMethod(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	err := cli.Invoke(context.Background(), "/api.EchoService/Nope", &api.EchoRequest{}, &api.EchoResponse{})
	if got := statusCodeOf(err); got != codes.Unimplemented {
		t.Fatalf("call to an unknown method = %v, want Unimplemented", err)
	}
	if !strings.Contains(err.Error(), "/api.EchoService/Nope") {
		t.Fatalf("Unimplemented error = %v, want it to carry the requested method name", err)
	}
}

// TestHandleReportsHandlerErrorAndKeepsServing pins the error boundary: a handler
// failure travels in the response header and the connection keeps serving the
// next call, so one bad request cannot take every other call down with it.
func TestHandleReportsHandlerErrorAndKeepsServing(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{failOn: "boom"}))
	echo := api.NewEchoServiceClient(cli)

	if _, err := echo.Echo(context.Background(), &api.EchoRequest{Message: "boom"}); statusCodeOf(err) != codes.InvalidArgument {
		t.Fatalf("failing call = %v, want InvalidArgument", err)
	}
	reply, err := echo.Echo(context.Background(), &api.EchoRequest{Message: "ok"})
	if err != nil {
		t.Fatalf("call after a handler error: %v", err)
	}
	if reply.Message != "ok!" {
		t.Fatalf("reply = %q, want %q", reply.Message, "ok!")
	}
}

// TestHandleSeesCallerMetadata pins the metadata hand-over through the real wire:
// the caller's pairs reach the interceptor's context, which is how auth and trace
// ids travel.
func TestHandleSeesCallerMetadata(t *testing.T) {
	var seen string
	var haveValue bool
	interceptor := func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		seen, haveValue = metadata.GetMetadataValue(ctx, "trace")
		if info.FullMethod != api.EchoService_Echo_FullMethodName {
			t.Errorf("interceptor saw method %q, want %q", info.FullMethod, api.EchoService_Echo_FullMethodName)
		}
		return handler(ctx, req)
	}
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}), UnaryServerInterceptor(interceptor))

	ctx := metadata.AppendToContext(context.Background(), "trace", "abc")
	if _, err := api.NewEchoServiceClient(cli).Echo(ctx, &api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if !haveValue || seen != "abc" {
		t.Fatalf("interceptor metadata trace = %q/%v, want abc/true", seen, haveValue)
	}
}

// TestHandleStopsOnContextCancel pins the shutdown path: cancelling the context
// closes the connection, so Handle returns an error instead of serving an idle
// peer forever, and no later call can reach the dead transport.
func TestHandleStopsOnContextCancel(t *testing.T) {
	cli, cancel, handleErr := startPipeServer(t, echoService(&echoImpl{}))
	if _, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}

	cancel()
	select {
	case err := <-handleErr:
		// A read blocked when the cancel lands reports the closed connection; the
		// Canceled branch only runs when the loop is idle. Either way the handler
		// names a failure, never a clean nil.
		if err == nil {
			t.Fatal("Handle returned nil after the connection was closed")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return within 2s of cancel")
	}

	if _, err := api.NewEchoServiceClient(cli).Echo(context.Background(), &api.EchoRequest{Message: "hi"}); err == nil {
		t.Fatal("a call after cancel succeeded on a closed transport")
	}
}

// TestHandleReportsMalformedHeader pins the framing guard: a request whose header
// does not decode ends the connection with an error rather than dispatching a
// call to a stream nobody opened.
func TestHandleReportsMalformedHeader(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	so := DefaultServerOptions
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errs := make(chan error, 1)
	go func() { errs <- so.Handle(ctx, srvConn) }()
	defer func() { _ = cliConn.Close() }()

	var h [messageHeaderLength]byte
	binary.BigEndian.PutUint32(h[:], 4)
	if _, err := cliConn.Write(append(h[:], 0xff, 0xff, 0xff, 0xff)); err != nil {
		t.Fatalf("write malformed header: %v", err)
	}

	select {
	case err := <-errs:
		if err == nil {
			t.Fatal("Handle accepted an undecodable header")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle did not return within 2s of a malformed header")
	}
}

// TestRegisterServiceOptionKeepsPreviousOnNil pins the option form: a nil
// descriptor or implementation leaves the registered service alone, so a
// conditional registration cannot wipe it.
func TestRegisterServiceOptionKeepsPreviousOnNil(t *testing.T) {
	so := DefaultServerOptions
	RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})(&so)
	RegisterService(nil, nil)(&so)
	if so.desc != &api.EchoService_ServiceDesc {
		t.Fatal("a nil descriptor replaced the registered one")
	}
	if _, ok := so.imp.(*echoImpl); !ok {
		t.Fatalf("implementation = %T, want *echoImpl", so.imp)
	}
}

// TestServerOptionsRegisterServiceReplaces pins the documented single-service
// rule: a later registration replaces the earlier one, because the dispatch table
// has a single entry.
func TestServerOptionsRegisterServiceReplaces(t *testing.T) {
	so := DefaultServerOptions
	RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})(&so)
	other := &grpc.ServiceDesc{ServiceName: "other.Service"}
	so.RegisterService(other, &echoImpl{})
	if so.desc != other {
		t.Fatalf("descriptor = %p, want the later registration %p", so.desc, other)
	}
	if so.desc.ServiceName != "other.Service" {
		t.Fatalf("service name = %q, want %q", so.desc.ServiceName, "other.Service")
	}
}

// TestDefaultServerOptionsInterceptorPassesThrough pins the default interceptor:
// with no interceptor configured the handler still runs, so a missing option
// never silently drops calls.
func TestDefaultServerOptionsInterceptorPassesThrough(t *testing.T) {
	so := DefaultServerOptions
	var ran bool
	reply, err := so.unaryInt(context.Background(), nil, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		ran = true
		return "reply", nil
	})
	if err != nil {
		t.Fatalf("default interceptor: %v", err)
	}
	if !ran {
		t.Fatal("default interceptor did not call the handler")
	}
	if reply != "reply" {
		t.Fatalf("reply = %v, want the handler's", reply)
	}
}

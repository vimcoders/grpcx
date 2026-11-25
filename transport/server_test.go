package transport

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// panicDesc is a service whose only method panics on the message "boom", so a
// test can prove the process survives a panicking handler.
var panicDesc = grpc.ServiceDesc{
	ServiceName: "test.PanicService",
	Methods: []grpc.MethodDesc{{
		MethodName: "Boom",
		Handler: func(_ any, _ context.Context, dec func(any) error, _ grpc.UnaryServerInterceptor) (any, error) {
			var req api.EchoRequest
			if err := dec(&req); err != nil {
				return nil, err
			}
			if req.Message == "boom" {
				panic("server boom")
			}
			return &api.EchoResponse{Message: req.Message + "!"}, nil
		},
	}},
}

// deadlineRecorder is an echo service that remembers the deadline its context
// carried, so a test can compare the server-side timeout with what the client
// asked for.
type deadlineRecorder struct {
	api.UnimplementedEchoServiceServer
	seenNs atomic.Int64
}

func (d *deadlineRecorder) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	if deadline, ok := ctx.Deadline(); ok {
		d.seenNs.Store(int64(time.Until(deadline)))
	}
	return &api.EchoResponse{Message: req.Message + "!"}, nil
}

// TestServerAnswersARegisteredMethod covers the dispatch loop end to end: a
// request naming a registered method reaches its implementation.
func TestServerAnswersARegisteredMethod(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))

	var reply api.EchoResponse
	if err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Errorf("reply.Message = %q, want hi!", reply.Message)
	}
}

// TestServerAnswersAnUnknownMethodWithUnimplemented covers the fallback: a
// service that does not name the method answers Unimplemented, and the
// connection keeps serving.
func TestServerAnswersAnUnknownMethodWithUnimplemented(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))

	var reply api.EchoResponse
	err := cli.Invoke(context.Background(), "/api.EchoService/Missing", &api.EchoRequest{Message: "hi"}, &reply)
	if code := status.Code(err); code != codes.Unimplemented {
		t.Errorf("unknown method = %v (%v), want Unimplemented", err, code)
	}

	// The second call proves the unknown method's payload was consumed too.
	if err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Errorf("call after Unimplemented = %v, want the connection to stay usable", err)
	}
}

// TestServerReportsAServiceErrorInTheReplyHeader covers a handler error: it
// becomes a status in the reply, not a broken connection.
func TestServerReportsAServiceErrorInTheReplyHeader(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{failOn: "boom"}))

	var reply api.EchoResponse
	err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "boom"}, &reply)
	if code := status.Code(err); code != codes.InvalidArgument {
		t.Errorf("failing call = %v (%v), want InvalidArgument", err, code)
	}
	if err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Errorf("call after a service error = %v, want the connection to stay usable", err)
	}
}

// TestServerSurvivesAPanickingHandler covers the isolation added for a
// panicking handler: the caller gets Internal and the next call is served.
func TestServerSurvivesAPanickingHandler(t *testing.T) {
	cli, _, _ := startPipeServer(t, RegisterService(&panicDesc, struct{}{}))

	var reply api.EchoResponse
	err := cli.Invoke(context.Background(), "/test.PanicService/Boom", &api.EchoRequest{Message: "boom"}, &reply)
	if code := status.Code(err); code != codes.Internal {
		t.Errorf("panicking call = %v (%v), want Internal", err, code)
	}

	if err := cli.Invoke(context.Background(), "/test.PanicService/Boom", &api.EchoRequest{Message: "ok"}, &reply); err != nil {
		t.Fatalf("call after a panic = %v, want it served", err)
	}
	if reply.Message != "ok!" {
		t.Errorf("reply.Message = %q, want ok!", reply.Message)
	}
}

// TestServerHandsItsHandlerTheClientDeadline covers the server side of the
// timeout: the deadline on the handler context is the one the request header
// carried.
func TestServerHandsItsHandlerTheClientDeadline(t *testing.T) {
	recorder := &deadlineRecorder{}
	cli, _, _ := startPipeServer(t, echoService(recorder))
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	var reply api.EchoResponse
	if err := cli.Invoke(ctx, "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	seen := time.Duration(recorder.seenNs.Load())
	if seen <= 0 || seen > 50*time.Millisecond {
		t.Errorf("handler deadline is %v away, want at most the 50ms the client asked for", seen)
	}
}

// TestServerFallsBackToItsTimeoutOption covers WithServerTimeout: a call whose
// header names no timeout runs under the server's option instead of the 3000ms
// default. RoundTrip is used because Invoke always writes its own default into
// the header, which would hide the fallback.
func TestServerFallsBackToItsTimeoutOption(t *testing.T) {
	recorder := &deadlineRecorder{}
	cli, _, _ := startPipeServer(t, echoService(recorder), WithServerTimeout(40*time.Millisecond))

	if _, err := cli.RoundTrip(context.Background(), &api.Request{
		Options: &api.CallOptions{FullMethodName: "/api.EchoService/Echo"},
		Payload: mustMarshal(t, &api.EchoRequest{Message: "hi"}),
	}); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	seen := time.Duration(recorder.seenNs.Load())
	if seen <= 0 || seen > 40*time.Millisecond {
		t.Errorf("handler deadline is %v away, want at most the 40ms the server option sets", seen)
	}
}

// TestInvokeFallsBackToItsTimeoutOption covers the Invoke side of the same
// fallback: with no deadline in ctx the header carries no timeout, so the server
// option decides. A ctx deadline still wins through the header, which
// TestServerHandsItsHandlerTheClientDeadline covers.
func TestInvokeFallsBackToItsTimeoutOption(t *testing.T) {
	recorder := &deadlineRecorder{}
	cli, _, _ := startPipeServer(t, echoService(recorder), WithServerTimeout(40*time.Millisecond))

	var reply api.EchoResponse
	if err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	seen := time.Duration(recorder.seenNs.Load())
	if seen <= 0 || seen > 40*time.Millisecond {
		t.Errorf("handler deadline is %v away, want at most the 40ms the server option sets", seen)
	}
}

// TestServerRunsTheInstalledInterceptor covers the interceptor option: it wraps
// every unary call.
func TestServerRunsTheInstalledInterceptor(t *testing.T) {
	var calls atomic.Int32
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}), UnaryServerInterceptor(
		func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
			calls.Add(1)
			return handler(ctx, req)
		}))

	var reply api.EchoResponse
	if err := cli.Invoke(context.Background(), "/api.EchoService/Echo", &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("interceptor ran %d times, want 1", got)
	}
}

// TestServerStopsWhenItsContextIsCanceled covers shutdown: canceling the context
// closes the connection under the read loop, so Handle returns instead of
// serving. Which error it reports depends on where the loop was parked: Canceled
// when it sees the context first, the broken-pipe error when it was already
// blocked in a read.
func TestServerStopsWhenItsContextIsCanceled(t *testing.T) {
	_, cancel, handleErr := startPipeServer(t, echoService(&echoImpl{}))
	cancel()

	select {
	case err := <-handleErr:
		if err == nil {
			t.Fatal("Handle returned nil after its context was canceled")
		}
		if c := status.Code(err); c != codes.Canceled && c != codes.Unknown {
			t.Errorf("Handle = %v (%v), want Canceled or the closed-connection error", err, c)
		}
	case <-time.After(time.Second):
		t.Fatal("Handle did not stop after its context was canceled")
	}
}

// TestServerStopsWhenTheConnectionBreaks covers a peer that goes away: the read
// error ends the loop.
func TestServerStopsWhenTheConnectionBreaks(t *testing.T) {
	cli, _, handleErr := startPipeServer(t, echoService(&echoImpl{}))
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	select {
	case err := <-handleErr:
		if err == nil {
			t.Error("Handle returned nil after the connection broke")
		}
	case <-time.After(time.Second):
		t.Fatal("Handle kept serving a broken connection")
	}
}

// TestRegisterServiceKeepsWhatItAlreadyHas covers the guard on the method: nil
// for either half leaves the recorded one in place, so a server built with one
// argument cannot wipe the other.
func TestRegisterServiceKeepsWhatItAlreadyHas(t *testing.T) {
	var so ServerOptions
	so.RegisterService(&api.EchoService_ServiceDesc, &echoImpl{})
	if so.desc == nil || so.imp == nil {
		t.Fatal("RegisterService did not record both halves")
	}
	desc, imp := so.desc, so.imp

	so.RegisterService(nil, nil)
	if so.desc != desc || so.imp != imp {
		t.Error("RegisterService(nil, nil) dropped an earlier registration")
	}

	so.RegisterService(desc, nil)
	if so.desc != desc {
		t.Error("RegisterService did not keep the descriptor it was handed")
	}
	if so.imp != imp {
		t.Error("RegisterService(desc, nil) dropped the implementation")
	}
}

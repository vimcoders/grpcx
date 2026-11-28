package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc/codes"
)

// dispatch runs one request through ServerOptions.interceptor over a
// fakeChannel: the header frame first, then the request payload.
func dispatch(t *testing.T, so ServerOptions, header *api.CallOptions, payload any) (*fakeChannel, []byte) {
	t.Helper()
	ch := &fakeChannel{}
	ch.recver.frames = []Buffer{headerFrame(t, header), payloadFrame(t, payload)}
	if err := so.interceptor(context.Background(), ch); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	frames := ch.sent()
	if len(frames) != 2 {
		t.Fatalf("sent %d frames, want a header and a payload", len(frames))
	}
	return ch, frames[1]
}

// replyHeader decodes the header frame one handler answered with.
func replyHeader(t *testing.T, frame []byte) *api.CallOptions {
	t.Helper()
	var header api.CallOptions
	if err := encoding.Unmarshal(frame, &header); err != nil {
		t.Fatalf("reply header decode: %v", err)
	}
	return &header
}

// TestInterceptorDispatchesRegisteredMethod covers the dispatch table: the
// request reaches the registered handler and its reply goes back over the same
// stream id.
func TestInterceptorDispatchesRegisteredMethod(t *testing.T) {
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, &echoImpl{}))
	header := &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName, StreamID: 5}
	_, body := dispatch(t, so, header, &api.EchoRequest{Message: "hi"})

	var resp api.EchoResponse
	if err := encoding.Unmarshal(body, &resp); err != nil {
		t.Fatalf("reply decode: %v", err)
	}
	if resp.Message != "hi!" {
		t.Errorf("reply = %q, want hi!", resp.Message)
	}
}

// TestInterceptorReportsHandlerError covers a failing handler: the status goes
// into the reply header and the connection stays up.
func TestInterceptorReportsHandlerError(t *testing.T) {
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, &echoImpl{failOn: "boom"}))
	header := &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName, StreamID: 5}
	ch, _ := dispatch(t, so, header, &api.EchoRequest{Message: "boom"})

	got := replyHeader(t, ch.sent()[0])
	if codes.Code(got.Code) != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", codes.Code(got.Code))
	}
	if got.Desc == "" {
		t.Error("the reply header carries no description")
	}
	if got.StreamID != 5 {
		t.Errorf("StreamID = %d, want 5", got.StreamID)
	}
}

// TestInterceptorRecoversPanics covers a panicking handler: it is answered
// with Internal instead of tearing the connection down.
func TestInterceptorRecoversPanics(t *testing.T) {
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, &echoImpl{panics: true}))
	header := &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName, StreamID: 5}
	ch, _ := dispatch(t, so, header, &api.EchoRequest{Message: "hi"})

	got := replyHeader(t, ch.sent()[0])
	if codes.Code(got.Code) != codes.Internal {
		t.Errorf("code = %v, want Internal", codes.Code(got.Code))
	}
}

// TestInterceptorReportsDeadline covers the server timeout: a budget that
// expires before the handler returns is reported as DeadlineExceeded.
func TestInterceptorReportsDeadline(t *testing.T) {
	so := serverOptions(
		RegisterService(&api.EchoService_ServiceDesc, &echoImpl{delay: 20 * time.Millisecond}),
		WithServerTimeout(time.Millisecond),
	)
	header := &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName, StreamID: 5}
	ch, _ := dispatch(t, so, header, &api.EchoRequest{Message: "hi"})

	got := replyHeader(t, ch.sent()[0])
	if codes.Code(got.Code) != codes.DeadlineExceeded {
		t.Errorf("code = %v, want DeadlineExceeded", codes.Code(got.Code))
	}
}

// TestInterceptorAnswersUnknownMethod covers the fallback: an unregistered
// name is answered with Unimplemented carrying the requested method, and the
// payload is still read so the connection stays aligned.
func TestInterceptorAnswersUnknownMethod(t *testing.T) {
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, &echoImpl{}))
	header := &api.CallOptions{FullMethodName: "/nope/X", StreamID: 9}
	ch, _ := dispatch(t, so, header, &api.EchoRequest{Message: "hi"})

	got := replyHeader(t, ch.sent()[0])
	if codes.Code(got.Code) != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", codes.Code(got.Code))
	}
	if got.Desc != "/nope/X" {
		t.Errorf("Desc = %q, want the requested method", got.Desc)
	}
}

// TestInterceptorKeepsAlignmentAcrossCalls covers two requests in a row: a
// rejected one does not leave the next frame misaligned.
func TestInterceptorKeepsAlignmentAcrossCalls(t *testing.T) {
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, &echoImpl{}))
	ch := &fakeChannel{}
	ch.recver.frames = []Buffer{
		headerFrame(t, &api.CallOptions{FullMethodName: "/nope/X", StreamID: 1}),
		payloadFrame(t, &api.EchoRequest{Message: "a"}),
		headerFrame(t, &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName, StreamID: 2}),
		payloadFrame(t, &api.EchoRequest{Message: "b"}),
	}
	for i := range 2 {
		if err := so.interceptor(context.Background(), ch); err != nil {
			t.Fatalf("interceptor %d: %v", i, err)
		}
	}
	if got := ch.recver.remaining(); got != 0 {
		t.Errorf("%d frames left, want none", got)
	}
	frames := ch.sent()
	if len(frames) != 4 {
		t.Fatalf("sent %d frames, want 4", len(frames))
	}
	var second api.EchoResponse
	if err := encoding.Unmarshal(frames[3], &second); err != nil {
		t.Fatalf("second reply decode: %v", err)
	}
	if second.Message != "b!" {
		t.Errorf("second reply = %q, want b!", second.Message)
	}
}

// TestHandleServesEchoOverPipe covers the full stack on a real connection: a
// client dials a pipe, the server dispatches, and the reply comes back.
func TestHandleServesEchoOverPipe(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	var resp api.EchoResponse
	if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &resp); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Message != "hi!" {
		t.Errorf("reply = %q, want hi!", resp.Message)
	}
}

// TestHandleServesRepeatedCalls covers the frame alignment of a real
// connection: several calls share one connection without stepping on each
// other's frames.
func TestHandleServesRepeatedCalls(t *testing.T) {
	cli, _, _ := startPipeServer(t, echoService(&echoImpl{}))
	for i := range 3 {
		var resp api.EchoResponse
		if err := cli.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &resp); err != nil {
			t.Fatalf("Invoke %d: %v", i, err)
		}
		if resp.Message != "hi!" {
			t.Errorf("reply %d = %q, want hi!", i, resp.Message)
		}
	}
}

// TestHandleStopsOnCancel covers the server loop: a canceled context ends
// Handle and closes its channel.
func TestHandleStopsOnCancel(t *testing.T) {
	so := serverOptions(echoService(&echoImpl{}))
	cliConn, srvConn := net.Pipe()
	t.Cleanup(func() { _ = cliConn.Close() })

	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- so.Handle(ctx, srvConn) }()
	cancel()

	select {
	case err := <-errs:
		if status.Code(err) != codes.Canceled {
			t.Errorf("code = %v, want Canceled", status.Code(err))
		}
	case <-time.After(time.Second):
		t.Fatal("Handle did not return after cancel")
	}
}

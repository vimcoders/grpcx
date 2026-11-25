package transport

import (
	"context"
	"errors"
	"math"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestCreateStreamAllocatesSequentialIDs pins how calls are numbered: ids start
// at 1 and never repeat, so a late frame cannot land on a newer call.
func TestCreateStreamAllocatesSequentialIDs(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithMaxStreams(4))
	for want := uint32(1); want <= 3; want++ {
		s, err := c.createStream(context.Background())
		if err != nil {
			t.Fatalf("createStream %d: %v", want, err)
		}
		if s.id != want {
			t.Fatalf("stream id = %d, want %d", s.id, want)
		}
		if got := c.getStream(want); got != s {
			t.Fatalf("stream %d is not in the table", want)
		}
	}
}

// TestCreateStreamRefusesAtMaxStreams pins the cap: once the table is full a
// call fails with ResourceExhausted instead of queueing, which is the signal the
// balancer uses to grow the pool.
func TestCreateStreamRefusesAtMaxStreams(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithMaxStreams(2))
	for i := range 2 {
		if _, err := c.createStream(context.Background()); err != nil {
			t.Fatalf("createStream %d: %v", i+1, err)
		}
	}
	if _, err := c.createStream(context.Background()); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("createStream on a full table = %v, want ResourceExhausted", err)
	}
}

// TestCreateStreamRefusesAfterClose pins the closed marker: Close zeroes the
// limit, and zero is not "no cap" but "closed", so a dead transport never hands
// out a stream.
func TestCreateStreamRefusesAfterClose(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if c.maxStreams != 0 {
		t.Fatalf("maxStreams = %d after Close, want 0", c.maxStreams)
	}
	if _, err := c.createStream(context.Background()); statusCodeOf(err) != codes.Unavailable {
		t.Fatalf("createStream on a closed transport = %v, want Unavailable", err)
	}
}

// TestCreateStreamRefusesIDWrapOntoLiveStream pins the wrap guard: an id counter
// at MaxUint32 wraps to 0, and a stream still holding that id must not be
// overwritten by the new call.
func TestCreateStreamRefusesIDWrapOntoLiveStream(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	c.streamID = math.MaxUint32
	c.streams[0] = &clientStream{id: 0}
	if _, err := c.createStream(context.Background()); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("createStream on an id wrap = %v, want ResourceExhausted", err)
	}
}

// TestNewStreamIsUnimplemented pins that grpcx carries unary calls only: the
// streaming entry point of grpc.ClientConnInterface refuses every request.
func TestNewStreamIsUnimplemented(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	if _, err := c.NewStream(context.Background(), nil, "/api.EchoService/Echo"); statusCodeOf(err) != codes.Unimplemented {
		t.Fatalf("NewStream = %v, want Unimplemented", err)
	}
}

// TestDeleteStreamRemovesTheEntry pins the deregistration: a stream removed from
// the table is no longer found, so no later frame is routed to it.
func TestDeleteStreamRemovesTheEntry(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithMaxStreams(2))
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("createStream: %v", err)
	}
	c.deleteStream(s.id)
	if got := c.getStream(s.id); got != nil {
		t.Fatalf("stream %d is still in the table after deleteStream", s.id)
	}
}

// TestInvokeSendsAndReturnsReply pins the unary path end to end over the transport
// surface: the request goes out under the method name, and the decoded reply comes
// back to the caller.
func TestInvokeSendsAndReturnsReply(t *testing.T) {
	c, a := newClientOnSender(t, replyEcho(t, codes.OK, codes.OK.String()))
	var reply api.EchoResponse
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Fatalf("reply = %q, want %q", reply.Message, "hi!")
	}

	header := requestHeader(t, a)
	if header.FullMethodName != api.EchoService_Echo_FullMethodName {
		t.Fatalf("header method = %q, want %q", header.FullMethodName, api.EchoService_Echo_FullMethodName)
	}
	if header.StreamID == 0 {
		t.Fatal("header carries no stream id")
	}
	if got := c.getStream(header.StreamID); got != nil {
		t.Fatalf("stream %d is still registered after Invoke returned", header.StreamID)
	}
}

// TestInvokeCarriesMetadataAndDefaultTimeout pins what the header advertises: the
// caller's metadata pairs and, without a deadline, the transport's default
// timeout, which is what the server arms its handler with.
func TestInvokeCarriesMetadataAndDefaultTimeout(t *testing.T) {
	c, a := newClientOnSender(t, replyEcho(t, codes.OK, codes.OK.String()))
	ctx := metadata.AppendToContext(context.Background(), "k", "v")
	var reply api.EchoResponse
	if err := c.Invoke(ctx, api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	header := requestHeader(t, a)
	if len(header.Metadatas) != 2 || header.Metadatas[0] != "k" || header.Metadatas[1] != "v" {
		t.Fatalf("header Metadatas = %v, want [k v]", header.Metadatas)
	}
	if header.Timeout != defaultTimeout.Milliseconds() {
		t.Fatalf("header timeout = %dms, want the default %dms", header.Timeout, defaultTimeout.Milliseconds())
	}
}

// TestInvokeFoldsDeadlineIntoHeader pins the deadline merge: a context deadline
// shorter than the transport default replaces it, so the server does not hold a
// handler past the caller's budget.
func TestInvokeFoldsDeadlineIntoHeader(t *testing.T) {
	c, a := newClientOnSender(t, replyEcho(t, codes.OK, codes.OK.String()))
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	var reply api.EchoResponse
	if err := c.Invoke(ctx, api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}

	header := requestHeader(t, a)
	if header.Timeout <= 0 || header.Timeout > 100 {
		t.Fatalf("header timeout = %dms, want the context deadline (<=100ms)", header.Timeout)
	}
}

// TestInvokeReportsSendError pins the failure path: when the connection cannot
// take the request the error reaches the caller instead of a wait for a reply
// that will never arrive.
func TestInvokeReportsSendError(t *testing.T) {
	want := errors.New("write failed")
	c, _ := newClientOnSender(t, func(*clientStream, *api.CallOptions, []byte) error {
		return want
	})
	var reply api.EchoResponse
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &reply); !errors.Is(err, want) {
		t.Fatalf("Invoke = %v, want %v", err, want)
	}
}

// TestInvokeRefusesClosedTransport pins that a call on a closed transport fails
// immediately with Unavailable rather than allocating a stream nothing will
// answer.
func TestInvokeRefusesClosedTransport(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	if err := c.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{}); statusCodeOf(err) != codes.Unavailable {
		t.Fatalf("Invoke on a closed transport = %v, want Unavailable", err)
	}
}

// TestRoundTripPreservesPayload pins the pre-framed path: the caller's bytes are
// written as they were handed in, under this transport's stream id, and the
// response comes back intact.
func TestRoundTripPreservesPayload(t *testing.T) {
	payload := []byte("already framed by the caller")
	c, a := newClientOnSender(t, func(s *clientStream, _ *api.CallOptions, _ []byte) error {
		s.c <- &api.Response{
			Options: &api.CallOptions{Code: uint32(codes.OK), Desc: codes.OK.String()},
			Payload: []byte("framed reply"),
		}
		return nil
	})

	req := &api.Request{Payload: payload}
	resp, err := c.RoundTrip(context.Background(), req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	if string(resp.Payload) != "framed reply" {
		t.Fatalf("response payload = %q, want %q", resp.Payload, "framed reply")
	}

	frames := a.sent()
	if len(frames) != 2 {
		t.Fatalf("RoundTrip wrote %d frames, want 2", len(frames))
	}
	if string(frames[1]) != string(payload) {
		t.Fatalf("sent payload = %q, want the caller's %q", frames[1], payload)
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.StreamID == 0 {
		t.Fatal("header carries no stream id")
	}
}

// TestRoundTripFoldsDeadlineIntoHeader pins the deadline on the pre-framed path
// too, because a caller that builds its own request must still get its budget
// honoured.
func TestRoundTripFoldsDeadlineIntoHeader(t *testing.T) {
	c, a := newClientOnSender(t, func(s *clientStream, _ *api.CallOptions, _ []byte) error {
		s.c <- &api.Response{Options: &api.CallOptions{Code: uint32(codes.OK)}, Payload: nil}
		return nil
	})
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	if _, err := c.RoundTrip(ctx, &api.Request{Payload: []byte("x")}); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	var header api.CallOptions
	if err := encoding.Unmarshal(a.sent()[0], &header); err != nil {
		t.Fatalf("decode header: %v", err)
	}
	if header.Timeout <= 0 || header.Timeout > 100 {
		t.Fatalf("header timeout = %dms, want the context deadline (<=100ms)", header.Timeout)
	}
}

// TestPingTreatsUnimplementedAsAlive pins the health check: Ping sends no method
// name, so the server can only answer Unimplemented, and that answer proves the
// connection is alive.
func TestPingTreatsUnimplementedAsAlive(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Unimplemented, "unimplemented"))
	if err := c.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v, want nil for an Unimplemented answer", err)
	}
}

// TestPingReportsOtherCodes pins that every other answer is a failure: a
// ResourceExhausted from the server means the connection cannot carry the call.
func TestPingReportsOtherCodes(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.ResourceExhausted, "too many streams"))
	if got := statusCodeOf(c.Ping(context.Background())); got != codes.ResourceExhausted {
		t.Fatalf("Ping = %v, want ResourceExhausted", got)
	}
}

// TestPingReportsCanceledStream pins the no-reply case: a stream closed before
// the answer arrives reports Canceled, which the balancer counts as a dead
// backend.
func TestPingReportsCanceledStream(t *testing.T) {
	c, _ := newClientOnSender(t, func(s *clientStream, _ *api.CallOptions, _ []byte) error {
		return s.Close()
	})
	if got := statusCodeOf(c.Ping(context.Background())); got != codes.Canceled {
		t.Fatalf("Ping = %v, want Canceled", got)
	}
}

// TestCloseCancelsEveryStreamOnce pins the shutdown: every waiting call is
// cancelled, the connection is closed exactly once, and a second Close is a no-op.
func TestCloseCancelsEveryStreamOnce(t *testing.T) {
	c, a := newClientOnSender(t, nil, WithMaxStreams(4))
	var wg sync.WaitGroup
	for range 2 {
		s, err := c.createStream(context.Background())
		if err != nil {
			t.Fatalf("createStream: %v", err)
		}
		wg.Go(func() {
			if got := statusCodeOf(s.RecvMsg(&api.EchoResponse{})); got != codes.Canceled {
				t.Errorf("waiting stream ended with %v, want Canceled", got)
			}
		})
	}

	if err := c.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	wg.Wait()
	if got := a.closeCount(); got != 1 {
		t.Fatalf("connection closed %d times, want 1", got)
	}
	if err := c.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
	if got := a.closeCount(); got != 1 {
		t.Fatalf("connection closed %d times after a second Close, want 1", got)
	}
}

// TestWithCodecReachesTheCall pins that the configured codec is the one a call
// decodes through, so a custom codec installed on the transport is not a field
// nothing reads.
func TestWithCodecReachesTheCall(t *testing.T) {
	codec := &countingCodec{}
	c, _ := newClientOnSender(t, replyEcho(t, codes.OK, codes.OK.String()), WithCodec(codec))
	var reply api.EchoResponse
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "hi"}, &reply); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if reply.Message != "hi!" {
		t.Fatalf("reply = %q, want %q", reply.Message, "hi!")
	}
	if got := codec.unmarshal.Load(); got == 0 {
		t.Fatal("the configured codec was never used to decode the reply")
	}
}

// TestDialReportsRefusedEndpoint pins the dial error path: DialContext surfaces a
// refused connection instead of returning a transport that cannot be used.
func TestDialReportsRefusedEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}

	if rt, err := Dial(addr); err == nil {
		_ = rt.Close()
		t.Fatalf("Dial(%s) = nil error, want a refusal", addr)
	}
}

// TestHandleReturnsCanceledWhenContextIsDone pins the loop's own exit: a context
// already cancelled when the loop starts reports Canceled and closes the
// connection, without reading a frame it would have to discard.
func TestHandleReturnsCanceledWhenContextIsDone(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	defer func() { _ = srvConn.Close() }()
	ch := newChannel(cliConn)
	c := &client{SendCloser: ch, Options: DefaultOptions(), streams: make(map[uint32]*clientStream)}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if got := statusCodeOf(c.Handle(ctx, ch)); got != codes.Canceled {
		t.Fatalf("Handle on a cancelled context = %v, want Canceled", got)
	}
}

// requestHeader decodes the header frame of the first recorded request.
func requestHeader(t *testing.T, a *answeringSender) *api.CallOptions {
	t.Helper()
	frames := a.sent()
	if len(frames) == 0 {
		t.Fatal("no request frame was recorded")
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("decode request header: %v", err)
	}
	return &header
}

var _ grpc.ClientConnInterface = (*client)(nil)

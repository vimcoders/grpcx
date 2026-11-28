package transport

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// TestCreateStreamAllocatesSequentialIDs covers the id allocator: each call
// claims the next id and registers it.
func TestCreateStreamAllocatesSequentialIDs(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	for want := uint32(1); want <= 3; want++ {
		s, err := c.createStream(context.Background())
		if err != nil {
			t.Fatalf("createStream: %v", err)
		}
		if s.id != want {
			t.Fatalf("id = %d, want %d", s.id, want)
		}
	}
	if got := len(c.streams); got != 3 {
		t.Errorf("streams = %d, want 3", got)
	}
}

// TestCreateStreamRejectsAtMaxStreams covers the cap: the call after the limit
// is refused with ResourceExhausted.
func TestCreateStreamRejectsAtMaxStreams(t *testing.T) {
	c, _ := newClientOnSender(t, nil, WithMaxStreams(2))
	for i := range 2 {
		if _, err := c.createStream(context.Background()); err != nil {
			t.Fatalf("createStream %d: %v", i, err)
		}
	}
	_, err := c.createStream(context.Background())
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", got)
	}
}

// TestCreateStreamRejectsAfterClose covers the closed marker: Close zeroes the
// limit, so every later call fails with Unavailable.
func TestCreateStreamRejectsAfterClose(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	_ = c.Close()
	if _, err := c.createStream(context.Background()); status.Code(err) != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", status.Code(err))
	}
	if got := len(c.streams); got != 0 {
		t.Errorf("streams = %d, want the table emptied", got)
	}
}

// TestCreateStreamRefusesALiveIDAfterWrap covers the wrap guard: an id that is
// still live is not overwritten.
func TestCreateStreamRefusesALiveIDAfterWrap(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	c.streamID = 1
	c.streams[2] = &clientStream{id: 2}
	_, err := c.createStream(context.Background())
	if got := status.Code(err); got != codes.ResourceExhausted {
		t.Errorf("code = %v, want ResourceExhausted", got)
	}
}

// TestCreateStreamClampsToCallerDeadline covers the nearer deadline: a caller
// deadline inside the read timeout wins.
func TestCreateStreamClampsToCallerDeadline(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("createStream: %v", err)
	}
	dl, ok := s.ctx.Deadline()
	if !ok {
		t.Fatal("the stream context has no deadline")
	}
	if until := time.Until(dl); until > c.readTimeout {
		t.Errorf("deadline in %v, want at most %v", until, c.readTimeout)
	}
}

// TestInvokeRoundTrips covers one unary call end to end over a fake
// connection: the request is encoded, framed and answered.
func TestInvokeRoundTrips(t *testing.T) {
	c, _ := newClientOnSender(t, replyEcho(t, codes.OK, ""))
	req := &api.EchoRequest{Message: "hi"}
	var resp api.EchoResponse
	if err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, req, &resp); err != nil {
		t.Fatalf("Invoke: %v", err)
	}
	if resp.Message != "hi!" {
		t.Errorf("reply = %q, want hi!", resp.Message)
	}
}

// TestInvokeReportsHandlerStatus covers a failing handler: the status code
// travels back in the reply header.
func TestInvokeReportsHandlerStatus(t *testing.T) {
	c, _ := newClientOnSender(t, replyEcho(t, codes.InvalidArgument, "bad"))
	var resp api.EchoResponse
	err := c.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &resp)
	if got := status.Code(err); got != codes.InvalidArgument {
		t.Errorf("code = %v, want InvalidArgument", got)
	}
}

// TestRoundTripFreesFailureReply covers the reply buffer of a failed call: it
// goes back to the pool instead of to the caller.
func TestRoundTripFreesFailureReply(t *testing.T) {
	buf := countingPayload([]byte("body"))
	respond := func(s *clientStream, _ *api.CallOptions, _ []byte) error {
		s.c <- &reply{
			CallOption: WithCallOptions(&api.CallOptions{Code: uint32(codes.Internal), Desc: "boom"}),
			Buffer:     buf,
		}
		return nil
	}
	c, _ := newClientOnSender(t, respond)
	_, err := c.RoundTrip(context.Background(), nil)
	if got := status.Code(err); got != codes.Internal {
		t.Errorf("code = %v, want Internal", got)
	}
	if got := buf.freeCount(); got != 1 {
		t.Errorf("reply freed %d times, want 1", got)
	}
}

// TestRoundTripHeaderCarriesDeadlineAndMetadata covers the two ctx inputs:
// the remaining deadline goes into Timeout, and the metadata pairs into
// Metadatas.
func TestRoundTripHeaderCarriesDeadlineAndMetadata(t *testing.T) {
	c, a := newClientOnSender(t, replyOnSlot(codes.OK, ""))
	ctx := metadata.WithMetadata(context.Background(), metadata.Pairs("k", "v"))
	ctx, cancel := context.WithTimeout(ctx, 50*time.Millisecond)
	defer cancel()
	if _, err := c.RoundTrip(ctx, nil); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	frames := a.sent()
	if len(frames) != 2 {
		t.Fatalf("sent %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if header.Timeout <= 0 || header.Timeout > 50 {
		t.Errorf("Timeout = %d, want a value inside the 50ms budget", header.Timeout)
	}
	if got, want := joinPairs(header.Metadatas), "k=v"; got != want {
		t.Errorf("Metadatas = %q, want %q", got, want)
	}
}

// TestRoundTripIgnoresCallOptionStreamID covers the one field the options
// cannot replace: the id createStream allocated is what goes on the wire, so a
// caller cannot point the frame at a stream this transport never registered.
func TestRoundTripIgnoresCallOptionStreamID(t *testing.T) {
	// The answering sender replies on the id it reads off the header, so a
	// replaced id would come back as Unavailable instead of a clean reply.
	c, a := newClientOnSender(t, replyOnSlot(codes.OK, ""))
	if _, err := c.RoundTrip(context.Background(), nil, &CallOptions{StreamID: 99}); err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}

	frames := a.sent()
	if len(frames) != 2 {
		t.Fatalf("sent %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if header.StreamID == 99 {
		t.Error("StreamID = 99, want the id this transport allocated")
	}
	if header.StreamID == 0 {
		t.Error("StreamID = 0, want a registered stream id")
	}
}

// TestPingSwallowsUnimplemented covers the health check's one exception: the
// server answering Unimplemented still proves the connection is alive.
func TestPingSwallowsUnimplemented(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Unimplemented, ""))
	if err := c.Ping(context.Background()); err != nil {
		t.Errorf("Ping = %v, want nil", err)
	}
}

// TestPingPropagatesOtherErrors covers every other status: a ping fails the
// caller so the balancer can drop the backend.
func TestPingPropagatesOtherErrors(t *testing.T) {
	c, _ := newClientOnSender(t, replyOnSlot(codes.Internal, "boom"))
	if err := c.Ping(context.Background()); status.Code(err) != codes.Internal {
		t.Errorf("code = %v, want Internal", status.Code(err))
	}
}

// TestCloseCancelsLiveStreams covers teardown: every live stream is canceled
// before the connection goes away.
func TestCloseCancelsLiveStreams(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("createStream: %v", err)
	}
	_ = c.Close()
	if s.Context().Err() == nil {
		t.Error("a live stream survived Close")
	}
}

// TestCloseClosesTheConnectionOnce covers the once: Close reaches the
// connection exactly once, however often it is called.
func TestCloseClosesTheConnectionOnce(t *testing.T) {
	c, a := newClientOnSender(t, nil)
	_ = c.Close()
	_ = c.Close()
	if got := a.closeCount(); got != 1 {
		t.Errorf("closed %d times, want 1", got)
	}
}

// TestNewStreamIsUnimplemented covers the unary-only transport: streaming is
// refused at the client interface.
func TestNewStreamIsUnimplemented(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	_, err := c.NewStream(context.Background(), &grpc.StreamDesc{}, "/x/Y")
	if got := status.Code(err); got != codes.Unimplemented {
		t.Errorf("code = %v, want Unimplemented", got)
	}
}

// TestHandleExitsOnCancel covers the receive loop: a canceled context ends it
// and closes the transport it owned.
func TestHandleExitsOnCancel(t *testing.T) {
	c, a := newClientOnSender(t, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := c.Handle(ctx, &fakeRecver{}); status.Code(err) != codes.Canceled {
		t.Errorf("code = %v, want Canceled", status.Code(err))
	}
	if got := a.closeCount(); got != 1 {
		t.Errorf("closed %d times after the loop exited, want 1", got)
	}
}

// TestHandleDiscardsAnUnroutableReply covers a reply whose stream is already
// gone: the payload is read off the wire to stay aligned and then freed.
func TestHandleDiscardsAnUnroutableReply(t *testing.T) {
	c, _ := newClientOnSender(t, nil)
	payload := countingPayload([]byte("orphan"))
	recv := &fakeRecver{frames: []Buffer{
		headerFrame(t, &api.CallOptions{StreamID: 404}),
		payload,
	}}
	if err := c.interceptor(context.Background(), recv); err != nil {
		t.Fatalf("interceptor: %v", err)
	}
	if got := payload.freeCount(); got != 1 {
		t.Errorf("orphan payload freed %d times, want 1", got)
	}
	if got := recv.remaining(); got != 0 {
		t.Errorf("%d frames left, want none", got)
	}
}

// TestDialContextChoosesTheCodec covers the client side of newCodec: a dial with
// batching on installs the unpooled codec, so a frame queued in the pending
// batch is never handed back to the pool before the flush.
func TestDialContextChoosesTheCodec(t *testing.T) {
	cases := []struct {
		name   string
		opts   []Option
		pooled bool
	}{
		{"batching off", nil, true},
		{"batching on", []Option{WithBufSize(1)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			ln, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatalf("listen: %v", err)
			}
			t.Cleanup(func() { _ = ln.Close() })
			go func() {
				for {
					conn, err := ln.Accept()
					if err != nil {
						return
					}
					go func(conn net.Conn) {
						defer func() { _ = conn.Close() }()
						buf := make([]byte, readBufSize)
						for {
							if _, err := conn.Read(buf); err != nil {
								return
							}
						}
					}(conn)
				}
			}()

			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			rt, err := DialContext(ctx, ln.Addr().String(), c.opts...)
			if err != nil {
				t.Fatalf("DialContext: %v", err)
			}
			t.Cleanup(func() { _ = rt.Close() })

			cl, ok := rt.(*client)
			if !ok {
				t.Fatalf("DialContext returned %T, want *client", rt)
			}
			if c.pooled {
				if _, ok := cl.Codec.(*defaultCodec); !ok {
					t.Fatalf("Codec is %T, want *defaultCodec", cl.Codec)
				}
				return
			}
			if _, ok := cl.Codec.(*emptyCodec); !ok {
				t.Fatalf("Codec is %T, want *emptyCodec", cl.Codec)
			}
		})
	}
}

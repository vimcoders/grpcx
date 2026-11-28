package transport

import (
	"context"
	"errors"
	"io"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc/codes"
)

// TestCallOptionsApplyOverridesNonZero covers the merge rule: a non-zero field
// replaces what an earlier option put there, a zero one leaves it alone, and
// Metadatas accumulates.
func TestCallOptionsApplyOverridesNonZero(t *testing.T) {
	var opts api.CallOptions
	base := CallOptions{FullMethodName: "/a/A", Timeout: 100, Metadatas: []string{"k", "v"}}
	base.apply(&opts)

	over := CallOptions{FullMethodName: "/b/B", Metadatas: []string{"x", "y"}}
	over.apply(&opts)
	if opts.FullMethodName != "/b/B" {
		t.Errorf("FullMethodName = %q, want /b/B", opts.FullMethodName)
	}
	if opts.Timeout != 100 {
		t.Errorf("Timeout = %d, want 100 left in place", opts.Timeout)
	}
	if got, want := joinPairs(opts.Metadatas), "k=v,x=y"; got != want {
		t.Errorf("Metadatas = %q, want %q", got, want)
	}
}

// TestWithCallOptionsCopiesFieldByField covers the adapter the dispatchers use
// to hand a decoded header to a handler: every field survives the round trip.
func TestWithCallOptionsCopiesFieldByField(t *testing.T) {
	in := &api.CallOptions{
		FullMethodName: "/api.EchoService/Echo",
		Service:        "EchoService",
		Timeout:        250,
		Code:           uint32(codes.OK),
		Desc:           "",
		StreamID:       7,
		Metadatas:      []string{"a", "b"},
	}
	var out api.CallOptions
	WithCallOptions(in).apply(&out)
	if out.FullMethodName != in.FullMethodName || out.Service != in.Service {
		t.Errorf("name = %q/%q, want %q/%q", out.FullMethodName, out.Service, in.FullMethodName, in.Service)
	}
	if out.Timeout != in.Timeout || out.StreamID != in.StreamID {
		t.Errorf("timeout/id = %d/%d, want %d/%d", out.Timeout, out.StreamID, in.Timeout, in.StreamID)
	}
	if got, want := joinPairs(out.Metadatas), "a=b"; got != want {
		t.Errorf("Metadatas = %q, want %q", got, want)
	}
}

// TestDecodeCallOptionsReadsTheHeader covers the request header decode: it uses
// the built-in codec and frees the frame it consumed.
func TestDecodeCallOptionsReadsTheHeader(t *testing.T) {
	in := &api.CallOptions{FullMethodName: "/api.EchoService/Echo", StreamID: 7, Timeout: 100}
	buf := countingPayload(mustMarshal(t, in))
	recv := &fakeRecver{frames: []Buffer{buf}}

	out, err := decodeCallOptions(recv)
	if err != nil {
		t.Fatalf("decodeCallOptions: %v", err)
	}
	if out.StreamID != 7 || out.Timeout != 100 {
		t.Errorf("header = %d/%d, want 7/100", out.StreamID, out.Timeout)
	}
	if got := buf.freeCount(); got != 1 {
		t.Errorf("frame freed %d times, want 1", got)
	}
}

// TestDecodeCallOptionsPropagatesReadError covers a broken connection: the read
// error is returned as it came.
func TestDecodeCallOptionsPropagatesReadError(t *testing.T) {
	recv := &fakeRecver{err: io.ErrUnexpectedEOF}
	if _, err := decodeCallOptions(recv); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Errorf("err = %v, want io.ErrUnexpectedEOF", err)
	}
}

// TestDecodeCallOptionsRejectsGarbage covers a header frame the codec cannot read.
func TestDecodeCallOptionsRejectsGarbage(t *testing.T) {
	recv := &fakeRecver{frames: []Buffer{emptyBuffer([]byte{0xff, 0xff, 0xff})}}
	if _, err := decodeCallOptions(recv); err == nil {
		t.Error("decodeCallOptions of garbage returned no error")
	}
}

// TestClientStreamHandleRoutesToSlot covers the demultiplexer's last hop: the
// payload lands in the slot the caller is parked on.
func TestClientStreamHandleRoutesToSlot(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	payload := countingPayload([]byte("reply"))
	recv := &fakeRecver{frames: []Buffer{payload}}

	if err := s.Handle(context.Background(), recv); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	select {
	case r := <-s.c:
		if got, want := string(r.ReadOnlyData()), "reply"; got != want {
			t.Errorf("payload = %q, want %q", got, want)
		}
		if got := payload.freeCount(); got != 0 {
			t.Errorf("frame freed %d times on the success path, want 0", got)
		}
	default:
		t.Fatal("Handle left the slot empty")
	}
}

// TestClientStreamHandleFreesAfterClose covers a reply that arrives after the
// call is gone: it goes back to the pool instead of into the slot.
func TestClientStreamHandleFreesAfterClose(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	_ = s.Close()
	payload := countingPayload([]byte("late"))

	if err := s.Handle(context.Background(), &fakeRecver{frames: []Buffer{payload}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := payload.freeCount(); got != 1 {
		t.Errorf("frame freed %d times, want 1", got)
	}
	if len(s.c) != 0 {
		t.Error("a late reply was delivered to the slot")
	}
}

// TestClientStreamHandleFreesWhenSlotIsFull covers the second drop branch: an
// occupied slot would never be read, so the frame goes back to the pool.
func TestClientStreamHandleFreesWhenSlotIsFull(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	s.c <- &reply{Buffer: emptyBuffer([]byte("occupied"))}
	payload := countingPayload([]byte("extra"))

	if err := s.Handle(context.Background(), &fakeRecver{frames: []Buffer{payload}}); err != nil {
		t.Fatalf("Handle: %v", err)
	}
	if got := payload.freeCount(); got != 1 {
		t.Errorf("frame freed %d times, want 1", got)
	}
}

// TestClientStreamRecvReportsCanceled covers the cancellation branch: a
// canceled context reports Canceled, whatever the reply would have been.
func TestClientStreamRecvReportsCanceled(t *testing.T) {
	s, _ := newStreamUnderTest(&fakeSender{})
	_ = s.Close()
	if _, err := s.recv(); status.Code(err) != codes.Canceled {
		t.Errorf("code = %v, want Canceled", status.Code(err))
	}
}

// TestClientStreamRecvReportsDeadline covers the other context branch: two
// deadlines land here and are reported as Unavailable.
func TestClientStreamRecvReportsDeadline(t *testing.T) {
	ctx, cancel := context.WithDeadline(context.Background(), time.Now())
	defer cancel()
	s := &clientStream{ctx: ctx, c: make(chan *reply, 1)}
	if _, err := s.recv(); status.Code(err) != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", status.Code(err))
	}
}

// TestClientStreamCloseSendDeregistersOnce covers the once: the stream leaves
// the table on the first close and stays gone on the second.
func TestClientStreamCloseSendDeregistersOnce(t *testing.T) {
	s, deleted := newStreamUnderTest(&fakeSender{})
	if err := s.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("second CloseSend: %v", err)
	}
	if got := deleted.Load(); got != 1 {
		t.Errorf("deregistered %d times, want 1", got)
	}
	if s.Context().Err() == nil {
		t.Error("CloseSend did not cancel the stream context")
	}
}

// TestSenderSendWritesHeaderThenPayload covers the wire shape of one message:
// the header frame first, then the payload, both read back through the codec.
func TestSenderSendWritesHeaderThenPayload(t *testing.T) {
	f := &fakeSender{}
	s := &sender{Sender: f, Codec: &defaultCodec{ecodec: protoCodec()}}

	opts := &CallOptions{StreamID: 3, Code: uint32(codes.OK)}
	if err := s.send(&api.EchoResponse{Message: "x"}, opts); err != nil {
		t.Fatalf("send: %v", err)
	}
	frames := f.sent()
	if len(frames) != 2 {
		t.Fatalf("sent %d frames, want 2", len(frames))
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(frames[0], &header); err != nil {
		t.Fatalf("header decode: %v", err)
	}
	if header.StreamID != 3 || header.Code != uint32(codes.OK) {
		t.Errorf("header = %d/%d, want 3/0", header.StreamID, header.Code)
	}
	var body api.EchoResponse
	if err := encoding.Unmarshal(frames[1], &body); err != nil {
		t.Fatalf("body decode: %v", err)
	}
	if body.Message != "x" {
		t.Errorf("body = %q, want x", body.Message)
	}
}

// TestSenderSendReportsMarshalError covers a codec that fails: no frame reaches
// the wire.
func TestSenderSendReportsMarshalError(t *testing.T) {
	f := &fakeSender{}
	s := &sender{Sender: f, Codec: codec{ecodec: fixedCodec{err: errors.New("boom")}}}
	if err := s.send(&api.Empty{}); err == nil {
		t.Fatal("send returned no error")
	}
	if got := len(f.sent()); got != 0 {
		t.Errorf("sent %d frames, want 0", got)
	}
}

// TestSenderSendPropagatesSendError covers a failing connection.
func TestSenderSendPropagatesSendError(t *testing.T) {
	boom := errors.New("bang")
	s := &sender{Sender: &fakeSender{err: boom}, Codec: &defaultCodec{ecodec: protoCodec()}}
	if err := s.send(&api.Empty{}); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

package transport

import (
	"encoding/binary"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/codes"
)

// TestChannelFramesRoundTrip pins the wire contract: every argument of Send
// becomes its own length-prefixed frame, and Recv strips the prefix and returns
// the payload. If the prefix or its width drifted, every peer would misread the
// stream from the first frame on.
func TestChannelFramesRoundTrip(t *testing.T) {
	a, b := tcpPair(t)
	sender, receiver := newChannel(a), newChannel(b)

	if err := sender.Send([]byte("hello"), []byte("world")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	for _, want := range []string{"hello", "world"} {
		p, err := receiver.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if got := string(p); got != want {
			t.Fatalf("Recv = %q, want %q", got, want)
		}
		putmbuf(p)
	}
}

// TestChannelZeroLengthFrame pins the empty-message path. An api.Empty marshals
// to zero bytes, so a frame with a zero length is the normal shape of a ping,
// and Recv must report it as a nil payload rather than an error.
func TestChannelZeroLengthFrame(t *testing.T) {
	a, b := tcpPair(t)
	sender, receiver := newChannel(a), newChannel(b)

	if err := sender.Send([]byte{}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	p, err := receiver.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if p != nil {
		t.Fatalf("Recv = %v, want nil payload", p)
	}
}

// TestChannelRejectsOversizePayload pins the caller-side cap: a payload above
// messageLengthMax is refused before anything reaches the wire, because the
// peer would reject the frame anyway.
func TestChannelRejectsOversizePayload(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(0))

	err := ch.Send(make([]byte, messageLengthMax+1))
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("Send oversize = %v, want OutOfRange", err)
	}
	if writes, _, _ := rec.stats(); writes != 0 {
		t.Fatalf("oversize frame wrote %d times, want 0", writes)
	}
}

// TestChannelRejectsOversizeHeader pins the reader-side cap. A peer announcing
// more than messageLengthMax bytes must be refused before the reader allocates
// the payload, or a single bogus header would cost 4GB of heap.
func TestChannelRejectsOversizeHeader(t *testing.T) {
	a, b := tcpPair(t)
	receiver := newChannel(b)

	var hdr [messageHeaderLength]byte
	binary.BigEndian.PutUint32(hdr[:], messageLengthMax+1)
	if _, err := a.Write(hdr[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	_, err := receiver.Recv()
	if status.Code(err) != codes.OutOfRange {
		t.Fatalf("Recv oversize header = %v, want OutOfRange", err)
	}
}

// TestChannelRecvTruncatedHeader pins the short-header case: a peer closing
// mid-header is a broken connection, reported as ErrUnexpectedEOF so a caller
// can tell it from a clean end of stream.
func TestChannelRecvTruncatedHeader(t *testing.T) {
	a, b := tcpPair(t)
	receiver := newChannel(b)

	if _, err := a.Write([]byte{0, 0}); err != nil {
		t.Fatalf("write partial header: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	_, err := receiver.Recv()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Recv truncated header = %v, want ErrUnexpectedEOF", err)
	}
}

// TestChannelRecvTruncatedPayload pins the short-payload case, and with it the
// buffer return: the partially filled payload must go back to the pool on the
// error path, which a leaked buffer would hide.
func TestChannelRecvTruncatedPayload(t *testing.T) {
	a, b := tcpPair(t)
	receiver := newChannel(b)

	var hdr [messageHeaderLength]byte
	binary.BigEndian.PutUint32(hdr[:], 8)
	if _, err := a.Write(append(hdr[:], []byte("abc")...)); err != nil {
		t.Fatalf("write truncated frame: %v", err)
	}
	if err := a.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	_, err := receiver.Recv()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("Recv truncated payload = %v, want ErrUnexpectedEOF", err)
	}
}

// TestChannelRecvArmsReadDeadlinePerFrame pins the idle guard and the second
// arm added for the payload: a frame is read under two deadlines, so a peer that
// sends a header and then stalls cannot hold the connection for twice the read
// timeout unnoticed. One deadline per frame would leave the payload unbounded.
func TestChannelRecvArmsReadDeadlinePerFrame(t *testing.T) {
	a, b := tcpPair(t)
	sender := newChannel(a)
	rec := &recordingConn{Conn: b}
	receiver := newChannel(rec)
	receiver.readTimeout = 200 * time.Millisecond

	for _, want := range []string{"one", "two"} {
		if err := sender.Send([]byte(want)); err != nil {
			t.Fatalf("Send: %v", err)
		}
		p, err := receiver.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		putmbuf(p)
	}
	read, _ := rec.deadlines()
	if len(read) != 4 {
		t.Fatalf("armed %d read deadlines for 2 frames, want 4 (header and payload each)", len(read))
	}
	for i, d := range read {
		if d <= 0 || d > receiver.readTimeout+time.Second {
			t.Fatalf("read deadline %d = %v, want ~%v", i, d, receiver.readTimeout)
		}
	}
}

// TestChannelBatchesUntilTimer pins the batching path: a frame below the buffer
// target is held until the flush interval elapses, which is what lets a burst of
// small messages share one syscall.
func TestChannelBatchesUntilTimer(t *testing.T) {
	a, b := tcpPair(t)
	sender := newChannel(a, WithBufSize(1))
	sender.flushInterval = 30 * time.Millisecond
	receiver := newChannel(b)

	if err := sender.Send([]byte("x")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	receiver.readTimeout = 10 * time.Millisecond
	if _, err := receiver.Recv(); err == nil {
		t.Fatal("Recv returned a frame before the flush interval elapsed, want a timeout")
	}

	receiver.readTimeout = 500 * time.Millisecond
	p, err := receiver.Recv()
	if err != nil {
		t.Fatalf("Recv after flush interval: %v", err)
	}
	if got := string(p); got != "x" {
		t.Fatalf("Recv = %q, want %q", got, "x")
	}
	putmbuf(p)
}

// TestChannelFlushesImmediatelyWhenBufferFilled pins the other batching branch:
// once the pending bytes reach bufsize the buffer goes out on the same call
// instead of waiting for the timer.
func TestChannelFlushesImmediatelyWhenBufferFilled(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(1))
	// Set the target below one frame so the very first Send crosses it. The
	// option itself takes blocks, so it cannot express a byte target this small.
	ch.bufsize = 8
	ch.flushInterval = time.Hour

	if err := ch.Send([]byte("abcde")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The frame is 4 header bytes plus the 5 byte payload. The count is on bytes
	// rather than calls because net.Buffers folds into one writev only for a
	// real TCP conn; against this sink it degrades to one Write per slice.
	writes, written, _ := rec.stats()
	if writes == 0 {
		t.Fatal("filling the buffer wrote nothing, want the frame on the wire")
	}
	if want := messageHeaderLength + len("abcde"); written != want {
		t.Fatalf("wrote %d bytes, want %d", written, want)
	}
}

// TestChannelSendArmsWriteDeadline pins that every Send arms its own write
// deadline. The flush timer inherits the deadline left by the last Send, so a
// Send that skipped it would let a batched write block forever.
func TestChannelSendArmsWriteDeadline(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(0))
	ch.writeTimeout = 50 * time.Millisecond

	for range 3 {
		if err := ch.Send([]byte("frame")); err != nil {
			t.Fatalf("Send: %v", err)
		}
	}
	_, write := rec.deadlines()
	if len(write) != 3 {
		t.Fatalf("armed %d write deadlines for 3 sends, want 3", len(write))
	}
	for i, d := range write {
		if d <= 0 || d > ch.writeTimeout {
			t.Fatalf("write deadline %d = %v, want ~%v", i, d, ch.writeTimeout)
		}
	}
}

// TestChannelSendReportsWriteError pins the error shape of a refused write: the
// caller sees both the underlying error and codes.Unavailable. The connection
// is deliberately left open — closing it here would hide the failure from the
// next call, which is the one that has to notice the socket is gone.
func TestChannelSendReportsWriteError(t *testing.T) {
	rec := &recordingConn{}
	rec.failWrites(errWriteRefused)
	ch := newChannel(rec, WithBufSize(0))

	err := ch.Send([]byte("frame"))
	if !errors.Is(err, errWriteRefused) {
		t.Fatalf("Send = %v, want it to carry errWriteRefused", err)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("status.Code(Send) = %v, want Unavailable", code)
	}
	if _, _, closed := rec.stats(); closed {
		t.Fatal("connection was closed by a failed write, want it left open")
	}
}

// TestChannelCloseClosesConnection pins the whole of Close: it closes the
// connection and nothing else. No flush is attempted and the connection is not
// closed twice, which is what the client's sync.Once relies on.
func TestChannelCloseClosesConnection(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(0))

	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, _, closed := rec.stats(); !closed {
		t.Fatal("Close left the connection open")
	}
}

// TestChannelCloseDropsPendingFrames pins the decision that Close does not
// flush: frames still in the batch buffer are dropped, so the peer sees the
// connection end rather than a final burst. Writing them out would make Close
// block on a socket the transport is already throwing away.
func TestChannelCloseDropsPendingFrames(t *testing.T) {
	a, b := tcpPair(t)
	sender := newChannel(a, WithBufSize(16))
	sender.flushInterval = time.Hour
	receiver := newChannel(b)
	receiver.readTimeout = 200 * time.Millisecond

	if err := sender.Send([]byte("dropped")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := receiver.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("Recv after Close = %v, want EOF and no frame", err)
	}
}

// TestChannelSendAfterCloseWrites pins that Send does not gate on a closed
// flag: after Close it still arms a deadline and attempts the write, then
// reports whatever the socket says. A local closed marker would have to be
// maintained by every caller, and this channel has none.
func TestChannelSendAfterCloseWrites(t *testing.T) {
	a, b := tcpPair(t)
	ch := newChannel(a, WithBufSize(0))

	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = b.Close()

	if err := ch.Send([]byte("late")); err == nil {
		t.Fatal("Send after Close = nil, want the socket error")
	}
}

// TestChannelTimerAfterCloseIsSilent pins that a flush timer firing after Close
// has nobody to report to and must not panic. The timer is intentionally not
// stopped by Close, so this path runs on every closed channel with a pending
// frame.
func TestChannelTimerAfterCloseIsSilent(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(16))
	ch.flushInterval = time.Millisecond

	if err := ch.Send([]byte("pending")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	// The timer runs its flush against the closed sink; a returned error there
	// is dropped by design. The wait is what would surface a panic or a deadlock.
	time.Sleep(5 * time.Millisecond)
}

// TestChannelConcurrentSendAndClose pins that Send and Close can interleave.
// Close takes no lock, so the guarantee under test is only that the two never
// deadlock or panic when they overlap. The race detector, which runs in CI, is
// what catches the memory-model side of it.
func TestChannelConcurrentSendAndClose(t *testing.T) {
	rec := &recordingConn{}
	ch := newChannel(rec, WithBufSize(1))

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			_ = ch.Send([]byte("concurrent"))
		})
	}
	wg.Go(func() {
		_ = ch.Close()
	})
	wg.Wait()
}

// TestChannelRecvReturnsTimeout pins the idle guard end to end: no frame
// arrives, so Recv reports the read deadline rather than blocking forever.
func TestChannelRecvReturnsTimeout(t *testing.T) {
	_, b := tcpPair(t)
	ch := newChannel(b)
	ch.readTimeout = 10 * time.Millisecond

	_, err := ch.Recv()
	if err == nil {
		t.Fatal("Recv on an idle connection = nil error, want a timeout")
	}
	var ne interface{ Timeout() bool }
	if !errors.As(err, &ne) || !ne.Timeout() {
		t.Fatalf("Recv idle = %v, want a timeout error", err)
	}
}

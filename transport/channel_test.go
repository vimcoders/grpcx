package transport

import (
	"encoding/binary"
	"errors"
	"io"
	"net"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc/codes"
)

// readFrame reads one length-prefixed frame from r.
func readFrame(t *testing.T, r io.Reader) []byte {
	t.Helper()
	var h [messageHeaderLength]byte
	if _, err := io.ReadFull(r, h[:]); err != nil {
		t.Fatalf("read header: %v", err)
	}
	n := binary.BigEndian.Uint32(h[:])
	p := make([]byte, n)
	if _, err := io.ReadFull(r, p); err != nil {
		t.Fatalf("read payload: %v", err)
	}
	return p
}

// writeFrame writes one length-prefixed frame to w. It returns the error rather
// than failing the test, because tests call it from a writer goroutine.
func writeFrame(w io.Writer, p []byte) error {
	var h [messageHeaderLength]byte
	binary.BigEndian.PutUint32(h[:], uint32(len(p)))
	_, err := w.Write(append(h[:], p...))
	return err
}

// isTimeout reports whether err is a read or write deadline expiring.
func isTimeout(err error) bool {
	return errors.Is(err, os.ErrDeadlineExceeded)
}

// TestSendWritesOneFramePerSlice covers the framing contract: every argument
// becomes one frame, in order, header first.
func TestSendWritesOneFramePerSlice(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	go func() {
		if err := ch.Send([]byte("ab"), []byte("cdef")); err != nil {
			t.Errorf("Send: %v", err)
		}
	}()
	if got := readFrame(t, srv); string(got) != "ab" {
		t.Errorf("first frame = %q, want ab", got)
	}
	if got := readFrame(t, srv); string(got) != "cdef" {
		t.Errorf("second frame = %q, want cdef", got)
	}
}

// TestSendRejectsOversizeFrame covers the cap: a slice above messageLengthMax
// is refused before anything reaches the wire.
func TestSendRejectsOversizeFrame(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	big := make([]byte, messageLengthMax+1)
	err := ch.Send(big)
	if err == nil {
		t.Fatal("Send of an oversize frame returned no error")
	}
	if got := status.Code(err); got != codes.OutOfRange {
		t.Errorf("code = %v, want OutOfRange", got)
	}
}

// TestSendFlushesAtThreshold covers the batching branch that writes at once:
// once the pending byte count reaches bufsize the frames go out inside Send.
func TestSendFlushesAtThreshold(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli, WithBufSize(16))

	done := make(chan error, 1)
	go func() { done <- ch.Send([]byte("0123456789ab")) }()
	// 4-byte header plus 12 bytes is 16, exactly the threshold, so the write
	// happens inside Send and the peer sees it at once.
	if got := readFrame(t, srv); string(got) != "0123456789ab" {
		t.Errorf("frame = %q, want 0123456789ab", got)
	}
	if err := <-done; err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// TestSendQueuesBelowThreshold covers the other batching branch: below the
// threshold Send returns without touching the wire. The peer does not read,
// so a synchronous write would park the call until the deadline.
func TestSendQueuesBelowThreshold(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli, WithBufSize(1024))

	done := make(chan error, 1)
	go func() { done <- ch.Send([]byte("ab")) }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Send: %v", err)
		}
	case <-time.After(50 * time.Millisecond):
		t.Fatal("Send parked on the wire, so it wrote synchronously instead of queueing")
	}

	// The flush timer runs at defaultFlushInterval, so the frame arrives well
	// inside this deadline.
	_ = srv.SetReadDeadline(time.Now().Add(500 * time.Millisecond))
	if got := readFrame(t, srv); string(got) != "ab" {
		t.Errorf("frame = %q, want ab", got)
	}
}

// TestCloseDropsPendingBatch covers Close's discard: a queued frame is never
// written, and the connection ends instead.
func TestCloseDropsPendingBatch(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, srv)
	ch := newChannel(cli, WithBufSize(1024))

	if err := ch.Send([]byte("ab")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	_ = srv.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var h [messageHeaderLength]byte
	if _, err := io.ReadFull(srv, h[:]); err == nil {
		t.Fatal("a frame was written after Close dropped the batch")
	}
}

// TestRecvReadsFrame covers the read path: the header is consumed here, the
// payload comes back as a Buffer the caller frees.
func TestRecvReadsFrame(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	go func() {
		if err := writeFrame(srv, []byte("hello")); err != nil {
			t.Errorf("write frame: %v", err)
		}
	}()
	p, err := ch.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer p.Free()
	if got, want := string(p.ReadOnlyData()), "hello"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}

	go func() {
		if err := writeFrame(srv, []byte("again")); err != nil {
			t.Errorf("write frame: %v", err)
		}
	}()
	p2, err := ch.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer p2.Free()
	if got, want := string(p2.ReadOnlyData()), "again"; got != want {
		t.Errorf("payload = %q, want %q", got, want)
	}
}

// TestRecvZeroLengthFrame covers the empty frame: it comes back as a non-nil
// Buffer so no caller has to nil-check, and Free is safe on it.
func TestRecvZeroLengthFrame(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	go func() {
		if err := writeFrame(srv, nil); err != nil {
			t.Errorf("write frame: %v", err)
		}
	}()
	p, err := ch.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if p == nil {
		t.Fatal("Recv returned a nil Buffer for an empty frame")
	}
	if len(p.ReadOnlyData()) != 0 {
		t.Errorf("payload length = %d, want 0", len(p.ReadOnlyData()))
	}
	p.Free()
}

// TestRecvRejectsOversizeHeader covers the header check: a length above
// messageLengthMax is refused before any payload is read.
func TestRecvRejectsOversizeHeader(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	var h [messageHeaderLength]byte
	binary.BigEndian.PutUint32(h[:], messageLengthMax+1)
	go func() { _, _ = srv.Write(h[:]) }()
	_, err := ch.Recv()
	if err == nil {
		t.Fatal("Recv of an oversize header returned no error")
	}
	if got := status.Code(err); got != codes.OutOfRange {
		t.Errorf("code = %v, want OutOfRange", got)
	}
}

// TestRecvArmsTheReadDeadline covers the idle-connection guard: with nothing
// on the wire, Recv gives up after readTimeout instead of waiting forever.
func TestRecvArmsTheReadDeadline(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli, WithReadTimeout(5*time.Millisecond))

	start := time.Now()
	_, err := ch.Recv()
	if err == nil {
		t.Fatal("Recv returned without a frame")
	}
	if !isTimeout(err) {
		t.Fatalf("error = %v, want a deadline", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("Recv waited %v, want about 5ms", elapsed)
	}
}

// TestConcurrentSendKeepsFramesAligned covers the lock: frames from many
// goroutines never interleave, and every header matches its payload length.
func TestConcurrentSendKeepsFramesAligned(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)

	const (
		writers = 8
		frames  = 3
	)
	var wg sync.WaitGroup
	wg.Add(writers)
	start := make(chan struct{})
	for i := range writers {
		go func(i int) {
			defer wg.Done()
			<-start
			for j := range frames {
				p := make([]byte, 16+i*frames+j)
				for k := range p {
					p[k] = byte('a' + i)
				}
				if err := ch.Send(p); err != nil {
					t.Errorf("Send: %v", err)
					return
				}
			}
		}(i)
	}
	close(start)

	got := 0
	for ; got < writers*frames; got++ {
		p := readFrame(t, srv)
		if len(p) == 0 {
			t.Fatal("read an empty frame")
		}
		for k, c := range p {
			if c != p[0] {
				t.Fatalf("frame %d interleaved two writers at byte %d", got, k)
			}
		}
	}
	wg.Wait()
	if got != writers*frames {
		t.Errorf("read %d frames, want %d", got, writers*frames)
	}
}

// TestChannelUsesAReadBufferOfThirtyTwoK covers the reader sizing: the bufio
// reader is min(messageLengthMax, readBufSize), not the frame cap.
func TestChannelUsesAReadBufferOfThirtyTwoK(t *testing.T) {
	cli, srv := net.Pipe()
	closeOnCleanup(t, cli, srv)
	ch := newChannel(cli)
	if got, want := ch.br.Size(), readBufSize; got != want {
		t.Errorf("reader size = %d, want %d", got, want)
	}
	_ = ch.Close()
}

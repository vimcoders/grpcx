package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"net"
	"os"
	"testing"
	"time"

	"google.golang.org/grpc/codes"
)

// TestNewChannelAppliesDefaults pins what a channel starts from: the packet
// defaults, no batching, and a reader sized at min(messageLengthMax,
// readBufSize). ServerOptions.Handle passes its own options on top, so a default
// that drifted would reach every accepted connection.
func TestNewChannelAppliesDefaults(t *testing.T) {
	conn, other := net.Pipe()
	defer func() { _ = other.Close() }()
	ch := newChannel(conn)
	defer func() { _ = ch.Close() }()

	want := DefaultOptions()
	if ch.timeout != want.timeout || ch.maxStreams != want.maxStreams {
		t.Fatalf("Options = %+v, want the defaults %+v", ch.Options, want)
	}
	if ch.readTimeout != want.readTimeout || ch.writeTimeout != want.writeTimeout {
		t.Fatalf("read/write timeout = %v/%v, want %v/%v", ch.readTimeout, ch.writeTimeout, want.readTimeout, want.writeTimeout)
	}
	if ch.bufsize != 0 {
		t.Fatalf("bufsize = %d, want 0 (batching off by default)", ch.bufsize)
	}
	if got, wantSize := ch.br.Size(), min(messageLengthMax, readBufSize); got != wantSize {
		t.Fatalf("read buffer = %d bytes, want %d", got, wantSize)
	}
}

// TestSendRecvFramesStayAligned pins the wire shape: every argument becomes one
// length-prefixed frame, and the receiver hands back exactly the payload without
// the header. Two frames in one call have to arrive as two frames.
func TestSendRecvFramesStayAligned(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	go func() {
		if err := cli.Send([]byte("header"), []byte("payload")); err != nil {
			t.Logf("Send: %v", err)
		}
	}()

	for _, want := range []string{"header", "payload"} {
		p, err := srv.Recv()
		if err != nil {
			t.Fatalf("Recv: %v", err)
		}
		if got := string(p); got != want {
			putmbuf(p)
			t.Fatalf("frame = %q, want %q", got, want)
		}
		putmbuf(p)
	}
}

// TestRecvZeroLengthFrame pins the empty-frame case: Ping sends api.Empty, which
// marshals to nothing, and a zero-length payload must come back as a nil buffer
// rather than a zero-length allocation. The header is written directly: a
// zero-length write is a no-op on TCP but blocks on net.Pipe until someone reads
// it, which would say nothing about framing.
func TestRecvZeroLengthFrame(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	go func() {
		var h [messageHeaderLength]byte
		binary.BigEndian.PutUint32(h[:], 0)
		if _, err := cli.Conn.Write(h[:]); err != nil {
			t.Logf("write header: %v", err)
		}
	}()

	p, err := srv.Recv()
	if err != nil {
		t.Fatalf("Recv on an empty frame: %v", err)
	}
	if len(p) != 0 {
		t.Fatalf("payload = %d bytes, want 0", len(p))
	}
}

// TestRecvRejectsOversizedHeader pins the frame cap: a header above
// messageLengthMax is refused before any payload is read, so a peer cannot make
// the reader allocate beyond the limit.
func TestRecvRejectsOversizedHeader(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	go func() {
		var h [messageHeaderLength]byte
		binary.BigEndian.PutUint32(h[:], uint32(messageLengthMax+1))
		if _, err := cli.Conn.Write(h[:]); err != nil {
			t.Logf("write header: %v", err)
		}
	}()

	p, err := srv.Recv()
	if p != nil {
		t.Fatalf("Recv returned a payload alongside %v", err)
	}
	if got := statusCodeOf(err); got != codes.OutOfRange {
		t.Fatalf("Recv on an oversized header = %v, want OutOfRange", err)
	}
}

// TestRecvSurfacesReadDeadline pins the idle-connection bound: with no frame
// arriving, Recv gives up after readTimeout instead of blocking forever. The
// balancer relies on that: a dead pod has to surface as a ping failure.
func TestRecvSurfacesReadDeadline(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(20*time.Millisecond), WithWriteTimeout(time.Second))
	defer func() { _ = cli.Close() }()

	start := time.Now()
	_, err := srv.Recv()
	if !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Recv = %v, want a deadline error", err)
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("Recv took %v to time out on a 20ms read timeout", elapsed)
	}
}

// TestRecvSurfacesTruncatedPayload pins the torn-frame case: a header that
// promises more bytes than arrive must fail the read rather than return a short
// payload, or the next frame would start off a boundary.
func TestRecvSurfacesTruncatedPayload(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	go func() {
		var h [messageHeaderLength]byte
		binary.BigEndian.PutUint32(h[:], 5)
		if _, err := cli.Conn.Write(append(h[:], 'a', 'b')); err != nil {
			t.Logf("write truncated frame: %v", err)
		}
		_ = cli.Close()
	}()

	p, err := srv.Recv()
	if p != nil {
		t.Fatalf("Recv returned a payload alongside %v", err)
	}
	if err == nil {
		t.Fatal("Recv on a truncated payload returned nil")
	}
}

// TestSendRejectsOversizedPayload pins the send-side cap: a frame above
// messageLengthMax is refused before anything is written, so the peer never sees
// a frame it would reject.
func TestSendRejectsOversizedPayload(t *testing.T) {
	cli, _ := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	p := make([]byte, messageLengthMax+1)
	if err := cli.Send(p); statusCodeOf(err) != codes.OutOfRange {
		t.Fatalf("Send of a %d-byte payload = %v, want OutOfRange", len(p), err)
	}
}

// TestSendBatchesBelowBufSize pins the batching threshold: a call whose pending
// bytes stay under bufsize returns without touching the connection, and the flush
// timer writes the frames out afterwards.
func TestSendBatchesBelowBufSize(t *testing.T) {
	cli, srv := pipeChannels(t,
		WithBufSize(defaultMaxBufSize/defaultBufSize),
		WithReadTimeout(time.Second),
		WithWriteTimeout(time.Second),
	)
	if err := cli.Send([]byte("tiny")); err != nil {
		t.Fatalf("Send: %v", err)
	}

	// Send returned with the frame still in the batch buffer, so this read is
	// served by the flush timer, not by the call.
	start := time.Now()
	p, err := srv.Recv()
	if err != nil {
		t.Fatalf("Recv after a buffered Send: %v", err)
	}
	defer putmbuf(p)
	if got := string(p); got != "tiny" {
		t.Fatalf("frame = %q, want %q", got, "tiny")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("the flush timer took %v to write a buffered frame", elapsed)
	}
}

// TestSendFlushesAtBufSize pins the other half of the threshold: once the pending
// bytes reach bufsize the call writes them itself, so the frame arrives without
// waiting for the timer.
func TestSendFlushesAtBufSize(t *testing.T) {
	cli, srv := pipeChannels(t, WithBufSize(1), WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	payload := bytes.Repeat([]byte("x"), defaultBufSize+1)
	go func() {
		if err := cli.Send(payload); err != nil {
			t.Logf("Send: %v", err)
		}
	}()

	p, err := srv.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	defer putmbuf(p)
	if !bytes.Equal(p, payload) {
		t.Fatalf("payload = %d bytes, want %d", len(p), len(payload))
	}
}

// TestCloseDropsBufferedFrames pins the documented close behaviour: frames still
// sitting in the batch buffer are dropped, so the peer sees the connection end
// without those bytes. Writing them out first would make Close wait on a peer.
func TestCloseDropsBufferedFrames(t *testing.T) {
	cli, srv := pipeChannels(t,
		WithBufSize(defaultMaxBufSize/defaultBufSize),
		WithReadTimeout(200*time.Millisecond),
		WithWriteTimeout(200*time.Millisecond),
	)
	if err := cli.Send([]byte("dropped")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	if p, err := srv.Recv(); err == nil {
		putmbuf(p)
		t.Fatalf("Recv after Close returned %q with no error", p)
	}
}

// TestCloseIsIdempotent pins that a second Close is harmless: the server calls it
// from a defer while the ctx AfterFunc may already have closed the same
// connection.
func TestCloseIsIdempotent(t *testing.T) {
	conn, other := net.Pipe()
	defer func() { _ = other.Close() }()
	ch := newChannel(conn)
	if err := ch.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := ch.Close(); err != nil {
		t.Fatalf("second Close = %v, want nil", err)
	}
}

// TestFlushTimerSwallowsWriteFailure pins the timer's failure path: a flush that
// fires after the peer went away is logged and dropped, because there is no
// caller left to report to. It must not panic or leave the channel usable.
func TestFlushTimerSwallowsWriteFailure(t *testing.T) {
	cli, srv := pipeChannels(t, WithBufSize(defaultMaxBufSize/defaultBufSize), WithWriteTimeout(time.Second))
	if err := cli.Send([]byte("pending")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	// The frame sits in the batch buffer, so the write happens in the timer. Closing
	// the peer first makes that write fail.
	if err := srv.Close(); err != nil {
		t.Fatalf("peer Close: %v", err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
}

// TestSendOnClosedConnectionReportsAnError pins that a Send after the connection
// is gone fails instead of pretending the frame went out.
func TestSendOnClosedConnectionReportsAnError(t *testing.T) {
	cli, _ := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if err := cli.Send([]byte("x")); err == nil {
		t.Fatal("Send on a closed connection returned nil")
	}
}

// TestSendReportsPeerCloseDuringWrite pins the other failure path: the peer goes
// away between the deadline and the write, so the write itself reports it.
func TestSendReportsPeerCloseDuringWrite(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(time.Second), WithWriteTimeout(time.Second))
	if err := srv.Close(); err != nil {
		t.Fatalf("peer Close: %v", err)
	}
	if err := cli.Send([]byte("x")); err == nil {
		t.Fatal("Send to a peer that closed its end returned nil")
	}
}

// TestBufferPoolRoundsUpToSizeClass pins getmbuf: the returned slice is at
// least the requested length, and putting it back lets the next caller reuse it
// instead of allocating. A frame above the largest size class is allocated fresh
// and must still be long enough.
func TestBufferPoolRoundsUpToSizeClass(t *testing.T) {
	for _, size := range []int{1, 1024, messageLengthMax} {
		p := getmbuf(size)
		if len(p) < size {
			t.Fatalf("getmbuf(%d) returned %d bytes", size, len(p))
		}
		putmbuf(p)
	}
}

// TestRecvArmsDeadlineForPayloadRead pins the second deadline: the header read
// already succeeded, so the payload read has to arm readTimeout again. A payload
// sharing the header's deadline would be cut short by whatever the header spent.
func TestRecvArmsDeadlineForPayloadRead(t *testing.T) {
	cli, srv := pipeChannels(t, WithReadTimeout(20*time.Millisecond), WithWriteTimeout(time.Second))
	go func() {
		var h [messageHeaderLength]byte
		binary.BigEndian.PutUint32(h[:], 8)
		if _, err := cli.Conn.Write(h[:]); err != nil {
			t.Logf("write header: %v", err)
		}
	}()

	if _, err := srv.Recv(); !errors.Is(err, os.ErrDeadlineExceeded) {
		t.Fatalf("Recv = %v, want a deadline error on the payload read", err)
	}
}

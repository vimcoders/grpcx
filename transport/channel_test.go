package transport

import (
	"encoding/binary"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc/codes"
)

// readFrames reads n length-prefixed frames off conn and returns their bodies
// concatenated, so a test can compare the bytes the peer actually got.
func readFrames(conn net.Conn, n int) (string, error) {
	var out []byte
	for range n {
		var head [messageHeaderLength]byte
		if _, err := io.ReadFull(conn, head[:]); err != nil {
			return "", err
		}
		body := make([]byte, binary.BigEndian.Uint32(head[:]))
		if _, err := io.ReadFull(conn, body); err != nil {
			return "", err
		}
		out = append(out, body...)
	}
	return string(out), nil
}

// TestChannelSendWritesLengthPrefixedFrames pins the wire shape: every argument
// becomes one frame with a big-endian length in front of it.
func TestChannelSendWritesLengthPrefixedFrames(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	srv := newChannel(srvConn)
	defer func() { _ = srv.Close() }()
	cli := newChannel(cliConn)
	defer func() { _ = cli.Close() }()

	frames := make(chan string, 1)
	go func() {
		body, err := readFrames(srvConn, 2)
		if err != nil {
			frames <- "error: " + err.Error()
			return
		}
		frames <- body
	}()

	if err := cli.Send([]byte("he"), []byte("llo")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case body := <-frames:
		if body != "hello" {
			t.Errorf("frames carried %q, want %q", body, "hello")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the frames never reached the peer")
	}
}

// TestChannelSendRejectsOversizedFrame documents messageLengthMax: a frame that
// large is refused before anything is written.
func TestChannelSendRejectsOversizedFrame(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	srv := newChannel(srvConn)
	defer func() { _ = srv.Close() }()
	cli := newChannel(cliConn)
	defer func() { _ = cli.Close() }()

	err := cli.Send(make([]byte, messageLengthMax+1))
	if code := status.Code(err); code != codes.OutOfRange {
		t.Errorf("Send(oversized) = %v (%v), want OutOfRange", err, code)
	}
}

// TestChannelSendReportsWriteFailure covers a connection that is already closed
// before the write starts: the deadline cannot be armed, the connection is
// closed again, and the joined status makes the failure Unavailable instead of
// Unknown.
func TestChannelSendReportsWriteFailure(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	srv := newChannel(srvConn)
	cli := newChannel(cliConn)
	_ = srv.Close()
	_ = cliConn.Close()

	err := cli.Send([]byte("x"))
	if err == nil {
		t.Fatal("Send on a closed connection returned nil")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("Send on a closed connection = %v (%v), want Unavailable", err, code)
	}
	_ = cli.Close()
}

// TestChannelSendMapsAWriteFailureToUnavailable covers the other failing point:
// the deadline is armed on a live connection and the write itself fails because
// the peer went away. Both failing points must report Unavailable, or a caller
// sees the same fault under two different codes.
func TestChannelSendMapsAWriteFailureToUnavailable(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newChannel(cliConn)
	_ = srvConn.Close()

	err := cli.Send([]byte("x"))
	if err == nil {
		t.Fatal("Send to a closed pipe returned nil")
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Errorf("Send to a closed pipe = %v (%v), want Unavailable", err, code)
	}
	_ = cli.Close()
}

// TestChannelRecvReturnsNilForZeroLengthFrame pins the meaning of a zero-length
// frame: it is delivered as a nil body, which is how the server answers a call
// with no payload.
func TestChannelRecvReturnsNilForZeroLengthFrame(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newChannel(cliConn)
	defer func() { _ = cli.Close() }()
	srv := newChannel(srvConn)
	defer func() { _ = srv.Close() }()

	go func() { _ = srv.Send(nil) }()
	body, err := cli.Recv()
	if err != nil {
		t.Fatalf("Recv: %v", err)
	}
	if body != nil {
		t.Errorf("Recv returned %v, want nil", body)
	}
}

// TestChannelRecvRejectsOversizedFrame covers a peer that announces more than
// messageLengthMax: the frame is dropped rather than allocated.
func TestChannelRecvRejectsOversizedFrame(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newChannel(cliConn)
	defer func() { _ = cli.Close() }()
	defer func() { _ = srvConn.Close() }()

	go func() {
		var head [messageHeaderLength]byte
		binary.BigEndian.PutUint32(head[:], uint32(messageLengthMax+1))
		_, _ = srvConn.Write(head[:])
	}()

	if _, err := cli.Recv(); status.Code(err) != codes.OutOfRange {
		t.Errorf("Recv(oversized) error = %v, want OutOfRange", err)
	}
}

// TestChannelRecvOnAClosedConnectionReportsTheDeadlineError covers the first
// guard of Recv: once the connection is gone, arming the read deadline fails and
// the error comes back before anything is read.
func TestChannelRecvOnAClosedConnectionReportsTheDeadlineError(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	_ = srvConn.Close()
	_ = cliConn.Close()
	cli := newChannel(cliConn)

	if _, err := cli.Recv(); err == nil {
		t.Fatal("Recv on a closed connection returned nil")
	}
}

// TestChannelRecvTimesOut covers the read timeout: a peer that stays silent
// makes the read fail instead of hanging the receive loop.
func TestChannelRecvTimesOut(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	defer func() { _ = srvConn.Close() }()
	cli := newChannel(cliConn, WithReadTimeout(20*time.Millisecond))
	defer func() { _ = cli.Close() }()

	done := make(chan error, 1)
	go func() {
		_, err := cli.Recv()
		done <- err
	}()
	select {
	case err := <-done:
		if err == nil {
			t.Error("Recv on a silent connection returned nil")
		}
	case <-time.After(time.Second):
		t.Fatal("Recv did not honour the read timeout")
	}
}

// TestChannelBatchesFramesUntilTheFlushTimer covers bufsize: below the byte
// threshold Send returns before the frame is on the wire, and the flush timer
// delivers it a moment later.
func TestChannelBatchesFramesUntilTheFlushTimer(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	srv := newChannel(srvConn)
	defer func() { _ = srv.Close() }()
	// One block is the threshold, so two small frames stay under it.
	cli := newChannel(cliConn, WithBufSize(1))
	defer func() { _ = cli.Close() }()

	frames := make(chan string, 1)
	go func() {
		body, err := readFrames(srvConn, 1)
		if err != nil {
			frames <- "error: " + err.Error()
			return
		}
		frames <- body
	}()

	if err := cli.Send([]byte("batched")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	select {
	case body := <-frames:
		if body != "batched" {
			t.Errorf("flush carried %q, want %q", body, "batched")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the flush timer never delivered the batched frame")
	}
}

// TestChannelCloseDropsTheBatch pins the shutdown order: closing does not flush
// what is buffered, it discards it, so a closed call never wakes up a peer.
func TestChannelCloseDropsTheBatch(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	defer func() { _ = srvConn.Close() }()
	cli := newChannel(cliConn, WithBufSize(1), WithWriteTimeout(50*time.Millisecond))

	if err := cli.Send([]byte("dropped")); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := cli.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	_ = srvConn.SetReadDeadline(time.Now().Add(100 * time.Millisecond))
	var head [messageHeaderLength]byte
	if _, err := io.ReadFull(srvConn, head[:]); err == nil {
		t.Error("the batched frame was flushed by Close")
	}
}

// TestChannelTimerFlushFailureClosesTheConnection covers the one write failure no
// caller hears: a queued batch is flushed by the timer to a peer that is already
// gone. Send reported success long before, so the batch is lost, and the close it
// leaves behind is the only signal — the next Send cannot arm its deadline, and a
// Recv waiting for a reply fails at once instead of sitting out the read timeout.
func TestChannelTimerFlushFailureClosesTheConnection(t *testing.T) {
	cliConn, srvConn := net.Pipe()
	cli := newChannel(cliConn, WithBufSize(1))
	defer func() { _ = cli.Close() }()

	// Below the threshold, so the call only queues the frame and reports success
	// while the peer is still there.
	if err := cli.Send([]byte("x")); err != nil {
		t.Fatalf("Send below the buffer: %v", err)
	}
	// The peer goes away before the flush timer fires.
	_ = srvConn.Close()
	time.Sleep(5 * defaultFlushInterval)

	// The failed flush closed the connection, so this Send fails arming its own
	// deadline. That is how the next caller learns about the batch it lost.
	if err := cli.Send([]byte("y")); err == nil {
		t.Fatal("Send after a failed timer flush returned nil")
	}
	// A caller already blocked on a reply hears the close too: Recv fails now
	// rather than waiting out its read deadline.
	if _, err := cli.Recv(); err == nil {
		t.Fatal("Recv on the connection a failed flush closed returned nil")
	}
}

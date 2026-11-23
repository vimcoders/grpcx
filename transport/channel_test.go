package transport

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/codes"
)

// TestChannelFramesRoundTrip sends two frames on one end and reads them back on
// the other, which is the exact wire shape a call produces: header first,
// payload second.
func TestChannelFramesRoundTrip(t *testing.T) {
	a, b := newTestConns(t)
	sender := newChannel(a)
	recver := newChannel(b)
	recver.readTimeout = 2 * time.Second

	header := []byte("header-bytes")
	payload := bytes.Repeat([]byte("p"), 300)
	if err := sender.Send(header, payload); err != nil {
		t.Fatalf("send: %v", err)
	}

	gotHeader, err := recver.Recv()
	if err != nil {
		t.Fatalf("recv header: %v", err)
	}
	defer putmbuf(gotHeader)
	if !bytes.Equal(gotHeader, header) {
		t.Fatalf("header = %q, want %q", gotHeader, header)
	}

	gotPayload, err := recver.Recv()
	if err != nil {
		t.Fatalf("recv payload: %v", err)
	}
	defer putmbuf(gotPayload)
	if !bytes.Equal(gotPayload, payload) {
		t.Fatalf("payload length = %d, want %d", len(gotPayload), len(payload))
	}
}

// TestChannelZeroLengthFrame pins that an empty frame comes back as a nil
// payload rather than a zero-length slice, which is what the empty handler and
// the keepalive rely on.
func TestChannelZeroLengthFrame(t *testing.T) {
	a, b := newTestConns(t)
	sender := newChannel(a)
	recver := newChannel(b)
	recver.readTimeout = 2 * time.Second

	if err := sender.Send(nil); err != nil {
		t.Fatalf("send: %v", err)
	}
	p, err := recver.Recv()
	if err != nil {
		t.Fatalf("recv: %v", err)
	}
	if p != nil {
		t.Fatalf("payload = %v, want nil", p)
	}
}

// TestChannelBatchesUntilTimer flushes buffered frames on the flush interval
// rather than on every Send, so a small write is not a small packet.
func TestChannelBatchesUntilTimer(t *testing.T) {
	a, b := newTestConns(t)
	sender := newChannel(a, WithBufSize(1))
	sender.flushInterval = 2 * time.Millisecond
	recver := newChannel(b)
	recver.readTimeout = 2 * time.Second

	if err := sender.Send([]byte("small")); err != nil {
		t.Fatalf("send: %v", err)
	}

	p, err := recver.Recv()
	if err != nil {
		t.Fatalf("recv after flush timer: %v", err)
	}
	defer putmbuf(p)
	if string(p) != "small" {
		t.Fatalf("payload = %q, want %q", p, "small")
	}
}

// TestChannelFlushesOnClose is the shutdown path: whatever batching still holds
// must reach the peer before the connection goes away, or the last reply of a
// call would be lost.
func TestChannelFlushesOnClose(t *testing.T) {
	a, b := newTestConns(t)
	sender := newChannel(a, WithBufSize(1))
	sender.flushInterval = time.Hour // only Close can flush this
	recver := newChannel(b)
	recver.readTimeout = 2 * time.Second

	if err := sender.Send([]byte("pending")); err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := sender.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	p, err := recver.Recv()
	if err != nil {
		t.Fatalf("recv after close: %v", err)
	}
	defer putmbuf(p)
	if string(p) != "pending" {
		t.Fatalf("payload = %q, want %q", p, "pending")
	}

	if _, err := recver.Recv(); !errors.Is(err, io.EOF) {
		t.Fatalf("recv at eof = %v, want io.EOF", err)
	}
}

// TestChannelRejectsOversizeFrame refuses to write a frame above the cap, and
// leaves nothing on the wire.
func TestChannelRejectsOversizeFrame(t *testing.T) {
	a, b := newTestConns(t)
	sender := newChannel(a)
	recver := newChannel(b)
	recver.readTimeout = 200 * time.Millisecond

	big := make([]byte, messageLengthMax+1)
	err := sender.Send(big)
	if code := status.Code(err); code != codes.OutOfRange {
		t.Fatalf("send oversize = %v, want %v", err, codes.OutOfRange)
	}

	if _, err := recver.Recv(); err == nil {
		t.Fatal("recv succeeded, want a timeout: the rejected frame must not reach the wire")
	}
}

// TestChannelRejectsOversizeHeader is the read side of the same cap: a peer
// announcing a frame above messageLengthMax is refused before the payload is
// read, so the allocator is never handed an attacker-chosen size.
func TestChannelRejectsOversizeHeader(t *testing.T) {
	a, b := newTestConns(t)
	recver := newChannel(b)
	recver.readTimeout = time.Second

	var hdr [messageHeaderLength]byte
	binary.BigEndian.PutUint32(hdr[:], uint32(messageLengthMax+1))
	if _, err := a.Write(hdr[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}

	_, err := recver.Recv()
	if code := status.Code(err); code != codes.OutOfRange {
		t.Fatalf("recv oversize header = %v, want %v", err, codes.OutOfRange)
	}
}

// TestChannelRecvTruncatedFrame reports a short body as a framing error, not as
// a valid frame, so the reader never treats garbage as a message.
func TestChannelRecvTruncatedFrame(t *testing.T) {
	a, b := newTestConns(t)
	recver := newChannel(b)
	recver.readTimeout = time.Second

	var hdr [messageHeaderLength]byte
	binary.BigEndian.PutUint32(hdr[:], 8)
	if _, err := a.Write(hdr[:]); err != nil {
		t.Fatalf("write header: %v", err)
	}
	if _, err := a.Write([]byte("abc")); err != nil {
		t.Fatalf("write body: %v", err)
	}
	_ = a.Close()

	_, err := recver.Recv()
	if !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("recv truncated = %v, want io.ErrUnexpectedEOF", err)
	}
}

// errTestWriteRefused is the write error failingWriteConn returns.
var errTestWriteRefused = errors.New("test: write refused")

// failingWriteConn accepts the deadline calls a channel makes and fails every
// write, the way a connection whose peer already went away does. Close succeeds,
// which is what used to hide the failure: the old code returned Close's error.
type failingWriteConn struct {
	net.Conn
	closed bool
}

func (c *failingWriteConn) Write([]byte) (int, error)        { return 0, errTestWriteRefused }
func (c *failingWriteConn) SetWriteDeadline(time.Time) error { return nil }
func (c *failingWriteConn) Close() error {
	c.closed = true
	return nil
}

// TestChannelSendReportsWriteError pins that a frame which never reached the wire
// is reported as a failed call carrying codes.Unavailable, and that the
// connection is closed so the next send cannot pretend it succeeded either.
func TestChannelSendReportsWriteError(t *testing.T) {
	conn := &failingWriteConn{}
	sender := newChannel(conn)

	err := sender.Send([]byte("header"), []byte("payload"))
	if err == nil {
		t.Fatal("send succeeded, want the write error")
	}
	if !errors.Is(err, errTestWriteRefused) {
		t.Fatalf("send = %v, want it to wrap %v", err, errTestWriteRefused)
	}
	if code := status.Code(err); code != codes.Unavailable {
		t.Fatalf("send code = %v, want %v", code, codes.Unavailable)
	}
	if !conn.closed {
		t.Fatal("connection left open after a failed write")
	}
}

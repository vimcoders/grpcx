package transport

import (
	"context"
	"errors"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
)

// errWriteRefused is the failure a test conn reports when it is told to reject
// every write. Tests assert it survives the wrapping, because a caller has to be
// able to tell a refused write from a transport-level status.
var errWriteRefused = errors.New("test: write refused")

// tcpPair returns two connected connections over the loopback interface. A real
// socket is required for the frame tests: net.Pipe is unbuffered, so a Write
// blocks until the peer reads, and an assertion about a completed write would
// deadlock instead of failing. t.Cleanup closes both.
func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	conn, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("dial: %v", err)
	}
	accepted, err := ln.Accept()
	if err != nil {
		_ = conn.Close()
		_ = ln.Close()
		t.Fatalf("accept: %v", err)
	}
	_ = ln.Close()
	t.Cleanup(func() {
		_ = conn.Close()
		_ = accepted.Close()
	})
	return conn, accepted
}

// recordingConn wraps a net.Conn and records what the channel does to it: how
// many writes reached the wire, how many bytes each one carried, whether the
// conn was closed, and every deadline the channel armed. Tests use it to pin
// the batching decisions and the deadline arming, which a plain pipe cannot
// observe.
type recordingConn struct {
	net.Conn
	mu             sync.Mutex
	writeCalls     int
	writeBytes     int
	closed         bool
	readDeadlines  []time.Duration
	writeDeadlines []time.Duration
	writeErr       error
}

// Write records the call and the byte count before delegating, so a test can see
// an attempted write even when the injection then fails it. A nil embedded conn
// makes the fake a sink: the bytes are counted and dropped, which lets a write
// test run without a peer that has to keep reading.
func (c *recordingConn) Write(b []byte) (int, error) {
	c.mu.Lock()
	c.writeCalls++
	c.writeBytes += len(b)
	err := c.writeErr
	inner := c.Conn
	c.mu.Unlock()
	if err != nil {
		return 0, err
	}
	if inner == nil {
		return len(b), nil
	}
	return inner.Write(b)
}

// Close records the close so a test can tell a closed conn from an abandoned one.
func (c *recordingConn) Close() error {
	c.mu.Lock()
	c.closed = true
	inner := c.Conn
	c.mu.Unlock()
	if inner == nil {
		return nil
	}
	return inner.Close()
}

// SetReadDeadline records how far ahead the deadline was armed. The interval is
// kept rather than the absolute time, because that is what the channel decides.
func (c *recordingConn) SetReadDeadline(t time.Time) error {
	c.mu.Lock()
	c.readDeadlines = append(c.readDeadlines, time.Until(t))
	inner := c.Conn
	c.mu.Unlock()
	if inner == nil {
		return nil
	}
	return inner.SetReadDeadline(t)
}

// SetWriteDeadline is the write half of SetReadDeadline.
func (c *recordingConn) SetWriteDeadline(t time.Time) error {
	c.mu.Lock()
	c.writeDeadlines = append(c.writeDeadlines, time.Until(t))
	inner := c.Conn
	c.mu.Unlock()
	if inner == nil {
		return nil
	}
	return inner.SetWriteDeadline(t)
}

func (c *recordingConn) stats() (writes, written int, closed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.writeCalls, c.writeBytes, c.closed
}

func (c *recordingConn) deadlines() (read, write []time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	return append([]time.Duration(nil), c.readDeadlines...), append([]time.Duration(nil), c.writeDeadlines...)
}

func (c *recordingConn) failWrites(err error) {
	c.mu.Lock()
	c.writeErr = err
	c.mu.Unlock()
}

// frameSource is a Recver backed by a fixed script of frames. It hands out the
// frames in order and then reports io.EOF, which is exactly how a peer closing
// the connection looks to the receive loop.
type frameSource struct {
	frames [][]byte
	err    error
	i      int
}

func (f *frameSource) Recv() ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.i >= len(f.frames) {
		return nil, io.EOF
	}
	p := f.frames[f.i]
	f.i++
	return p, nil
}

// newFrame copies payload into a pooled buffer, which is what channel.Recv
// returns in production. The copy matters: the callers put the buffer back with
// putmbuf, and that pool only recycles buffers whose capacity matches a size
// class.
func newFrame(payload []byte) []byte {
	p := getmbuf(len(payload))
	copy(p, payload)
	return p
}

// recordSender is a Sender that keeps every frame group it was handed and can
// be told to fail. The frames are copied because the channel reuses its own
// header buffers across calls.
type recordSender struct {
	mu    sync.Mutex
	calls [][][]byte
	err   error
}

func (s *recordSender) Send(p ...[]byte) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.err != nil {
		return s.err
	}
	group := make([][]byte, len(p))
	for i, b := range p {
		group[i] = append([]byte(nil), b...)
	}
	s.calls = append(s.calls, group)
	return nil
}

func (s *recordSender) count() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.calls)
}

func (s *recordSender) frame(g, i int) []byte {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls[g][i]
}

func (s *recordSender) fail(err error) {
	s.mu.Lock()
	s.err = err
	s.mu.Unlock()
}

// mustMarshal marshals a message through the codec the transport uses, failing
// the test rather than returning an error a frame-level test cannot act on.
func mustMarshal(t *testing.T, msg any) []byte {
	t.Helper()
	b, err := encoding.Marshal(msg)
	if err != nil {
		t.Fatalf("marshal %T: %v", msg, err)
	}
	return b
}

// headerFrame builds the frame a peer sends ahead of a payload: the marshalled
// call options.
func headerFrame(t *testing.T, opts *api.CallOptions) []byte {
	t.Helper()
	return newFrame(mustMarshal(t, opts))
}

// decodeHeader is the inverse of headerFrame, for the tests that inspect the
// header a sender wrote.
func decodeHeader(t *testing.T, frame []byte) *api.CallOptions {
	t.Helper()
	var opts api.CallOptions
	if err := encoding.Unmarshal(frame, &opts); err != nil {
		t.Fatalf("unmarshal call options: %v", err)
	}
	return &opts
}

// testContext returns a context whose cancellation the test controls, plus the
// cleanup that cancels it. Every test that parks a goroutine on a request uses
// it so a failure cannot leave the goroutine running into the next test.
func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	return ctx
}

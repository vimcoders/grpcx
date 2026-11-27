/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

package transport

import (
	"bufio"
	"encoding/binary"
	"errors"
	"io"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/mem"
)

// defaultBuffer is a frame payload whose storage comes from buffers. Free
// returns it to the pool and must be called exactly once.
type defaultBuffer []byte

func (e defaultBuffer) Free() {
	putmbuf(e)
}

func (e defaultBuffer) ReadOnlyData() []byte {
	return e
}

// buffers is the process-wide pool every frame payload is taken from, so a
// payload is recycled instead of allocated per call. The pool tops out at its
// largest size class; a frame above that is allocated fresh.
var buffers = mem.DefaultBufferPool()

// getmbuf returns a pooled buffer of at least size bytes. The pool rounds
// the request up to its next size class, so the result can be longer.
func getmbuf(size int) defaultBuffer {
	// we can't use the standard New method on pool because we want to allocate
	// based on size.
	return *buffers.Get(size)
}

// putmbuf returns a buffer obtained from getmbuf to the pool.
func putmbuf(p []byte) {
	buffers.Put(&p)
}

// channel owns one net.Conn: it frames outbound messages, reads inbound frames,
// and batches writes when bufsize is non-zero.
type channel struct {
	Options
	net.Conn
	br *bufio.Reader
	bw net.Buffers
	t  *time.Timer
	sync.Mutex
}

// newChannel wraps c with a buffered reader and applies opts on top of the
// defaults.
func newChannel(c net.Conn, opts ...Option) *channel {
	defaultOptions := DefaultOptions()
	for _, opt := range opts {
		opt(&defaultOptions)
	}
	x := &channel{
		Conn:    c,
		br:      bufio.NewReaderSize(c, min(messageLengthMax, readBufSize)),
		Options: defaultOptions,
	}
	// push is the timer flush. It writes the pending batch under the deadline
	// the last Send armed. A failure closes the connection: Send already
	// returned nil for the frames in that batch, so they are lost, and the
	// close is the only signal left — the read side fails at once instead of
	// sitting out the read timeout. The error is not logged here; the next
	// Send reports a closed connection of its own.
	push := func() {
		x.Lock()
		defer x.Unlock()
		if _, err := x.bw.WriteTo(x.Conn); err != nil {
			_ = x.Conn.Close()
		}
	}
	x.t = time.AfterFunc(defaultFlushInterval, push)
	return x
}

// Recv reads one frame: a 4-byte length header, then the payload. The stream id
// is not in the frame header; it travels inside the protobuf header frame.
//
// Only the header read arms the read deadline. The payload read shares it, so
// a slow large payload spends the rest of the same budget.
//
// The payload comes from the pool and the caller owns it: Free returns it.
// A zero-length frame returns an empty Buffer, not nil, so every call has
// something to free and no caller has to nil-check.
func (ch *channel) Recv() (Buffer, error) {
	var hrbuf [messageHeaderLength]byte // avoid alloc when reading header

	if err := ch.SetReadDeadline(time.Now().Add(ch.readTimeout)); err != nil {
		return nil, err
	}

	if _, err := io.ReadFull(ch.br, hrbuf[:]); err != nil {
		return nil, err
	}

	h := binary.BigEndian.Uint32(hrbuf[:])

	if h > uint32(messageLengthMax) {
		return nil, status.OutOfRange.Err()
	}

	if h > 0 {
		p := getmbuf(int(h))
		if _, err := io.ReadFull(ch.br, p); err != nil {
			p.Free()
			return nil, err
		}
		return p, nil
	}

	return &defaultBuffer{}, nil
}

// Send writes each slice as one frame: a 4-byte length header, then the bytes.
// A caller sends a call as two frames — the marshalled header and the payload.
//
// With bufsize == 0 (the default) the write completes inside this call, and
// the slices are free to reuse when it returns.
//
// With bufsize > 0 the frames are appended to the pending batch by reference,
// not copied. They are written when the batch reaches bufsize, or by the timer
// after defaultFlushInterval. The caller's slices must not change until that
// write happens.
//
// The write deadline is armed once, at the top of the call, before any frame
// is queued, so both paths write under a deadline that was just set.
//
// A failed write closes the connection and returns the I/O error joined with
// the Close error and codes.Unavailable. Frames left in the batch are dropped:
// the connection is closed, so no later flush can write them.
//
// The timer flush has no caller to report to. Send already returned nil for
// those frames, so a failure there only closes the connection: the next Send
// fails arming its deadline, and a call already waiting for a reply wakes on
// the closed connection. bufsize == 0 is the only setting where every write
// failure reaches its caller.
func (ch *channel) Send(p ...[]byte) error {
	var bw net.Buffers
	for _, b := range p {
		if len(b) > messageLengthMax {
			return status.OutOfRange.Err()
		}
		var hwbuf [messageHeaderLength]byte
		binary.BigEndian.PutUint32(hwbuf[:], uint32(len(b)))
		bw = append(bw, hwbuf[:], b)
	}

	ch.Lock()
	defer ch.Unlock()
	if err := ch.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
		return errors.Join(err, ch.Conn.Close(), status.Unavailable.Err())
	}
	var n int
	ch.bw = append(ch.bw, bw...)
	for _, b := range ch.bw {
		n += len(b)
	}
	if n < ch.bufsize {
		_ = ch.t.Reset(defaultFlushInterval)
		return nil
	}
	defer ch.t.Stop()
	if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
		return errors.Join(err, ch.Conn.Close(), status.Unavailable.Err())
	}
	return nil
}

// Close closes the connection under the same lock as Send, so no flush is
// in flight while the connection goes away, and stops the flush timer: frames
// still in the batch are dropped, not written. The client calls it from a
// sync.Once after canceling every live stream; the server calls it on
// connection end.
func (ch *channel) Close() error {
	ch.Lock()
	defer ch.Unlock()
	defer ch.t.Stop()
	return ch.Conn.Close()
}

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
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/mem"
)

// buffers is the process-wide buffer pool every frame payload is allocated
// from, so a payload is recycled instead of allocated per call. The pool tops
// out at its largest size class (1MB); a frame above that is allocated fresh.
var buffers = mem.DefaultBufferPool()

// getmbuf returns a buffer from the pool, guaranteed to be at least size bytes
// long. The pool rounds up to its own size classes.
func getmbuf(size int) []byte {
	// we can't use the standard New method on pool because we want to allocate
	// based on size.
	return *buffers.Get(size)
}

// putmbuf returns a buffer obtained from getmbuf to the pool.
func putmbuf(p []byte) {
	buffers.Put(&p)
}

// channel owns one net.Conn: it frames messages, allocates payloads from the
// pool, and batches writes when a non-zero bufsize is configured.
type channel struct {
	Options
	net.Conn
	br *bufio.Reader
	bw net.Buffers
	t  *time.Timer
	sync.Mutex
}

// newChannel wraps c with a buffered reader and applies the options on top of
// the defaults.
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
	push := func() {
		x.Lock()
		defer x.Unlock()
		if _, err := x.bw.WriteTo(x.Conn); err != nil {
			slog.Debug("channel push", "err", err.Error())
		}
	}
	x.t = time.AfterFunc(x.flushInterval, push)
	return x
}

// Recv reads one frame: a 4-byte header carrying the payload length, followed
// by the payload itself. The stream id is not in the frame header; it travels
// inside the protobuf header frame that Recv returns first. Only the header read
// is armed with readTimeout: the payload read shares that deadline, so a large
// payload arriving slowly spends the rest of the same budget. The payload comes
// from the pool and the caller must putmbuf it. A zero-length frame returns a nil
// payload.
func (ch *channel) Recv() ([]byte, error) {
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
		payload := getmbuf(int(h))
		if _, err := io.ReadFull(ch.br, payload); err != nil {
			putmbuf(payload)
			return nil, err
		}
		return payload, nil
	}

	return nil, nil
}

// Send writes the given byte slices as one or more frames, each prefixed with
// a 4-byte payload length. Callers pass the header slice and the payload slice
// as separate frames; the stream id lives in the header slice, not the frame
// header. Every call arms the write deadline before it touches the connection.
//
// With bufsize <= 0 every call goes straight to the connection. Otherwise the
// frames are appended to a pending write buffer and flushed as soon as it grows
// past bufsize, or by a timer after flushInterval, whichever comes first. The
// timer does not arm a deadline of its own: it writes under the deadline the last
// Send installed. A write that fails is returned as it is, unwrapped; the
// connection is left in place for the next call to discover.
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
	var n int
	ch.bw = append(ch.bw, bw...)
	for _, b := range ch.bw {
		n += len(b)
	}
	if n < ch.bufsize {
		_ = ch.t.Reset(ch.flushInterval)
		return nil
	}

	if err := ch.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
		return errors.Join(err, status.Unavailable.Err())
	}
	defer ch.t.Stop()
	if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
		return errors.Join(err, status.Unavailable.Err())
	}
	return nil
}

// Close closes the connection. It takes the same lock as Send, so a flush cannot
// be in flight while the connection goes away, and it stops the flush timer:
// frames still sitting in the batch buffer are dropped rather than written out.
// The client calls it from sync.Once when the transport goes down, leaving the
// streams on it to their own deadlines; the server calls it from a defer when the
// connection ends.
func (ch *channel) Close() error {
	ch.Lock()
	defer ch.Unlock()
	defer ch.t.Stop()
	return ch.Conn.Close()
}

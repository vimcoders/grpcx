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
	return x
}

// Recv reads one frame: a 4-byte header carrying the payload length, followed
// by the payload itself. The stream id is not in the frame header; it travels
// inside the protobuf header frame that Recv returns first. The payload comes
// from the pool and the caller must putmbuf it. A zero-length frame returns a
// nil payload.
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
		if err := ch.SetReadDeadline(time.Now().Add(ch.readTimeout)); err != nil {
			return nil, err
		}
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
// past bufsize, or by a timer after flushInterval, whichever comes first. A write
// that fails is reported as Unavailable; the connection is left in place for the
// next call to discover.
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
	if err := ch.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
		return err
	}

	if n >= ch.bufsize {
		if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
			return errors.Join(err, status.Unavailable.Err())
		}
		if ch.t != nil {
			ch.t.Stop()
		}
		return nil
	}

	if ch.t != nil {
		ch.t.Stop()
		ch.t.Reset(ch.flushInterval)
		return nil
	}
	ch.t = time.AfterFunc(ch.flushInterval, func() {
		ch.Lock()
		defer ch.Unlock()
		// The timer has nobody to report to, so a failed flush ends here: the
		// socket is left open and the next Send is the one that sees it broke.
		if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
			return
		}
	})

	return nil
}

// Close closes the connection. Frames still sitting in the batch buffer are
// dropped rather than written out, and the flush timer is left running: if it
// fires after this call its write fails silently against the closed socket.
// Close takes no lock of its own. The client calls it from sync.Once, after every
// stream has been cancelled.
func (ch *channel) Close() error {
	return ch.Conn.Close()
}

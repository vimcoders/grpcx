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
	"io"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc/mem"
)

// Sender is the interface for sending messages to a channel. It is kept for
// compatibility; the transport talks to the concrete *channel directly.
type Sender interface {
	Send(uint32, ...[]byte) error
	io.Closer
}

// buffers is the process-wide buffer pool every frame payload is allocated
// from, so a 1MB payload is recycled instead of allocated per call.
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
		br:      bufio.NewReader(c),
		Options: defaultOptions,
	}
	return x
}

// Recv reads one frame: an 8-byte header carrying the stream id and the
// payload length, followed by the payload itself. The payload comes from the
// pool and the caller must putmbuf it. A zero-length frame returns a nil
// payload.
func (ch *channel) Recv() (uint32, []byte, error) {
	var hrbuf [messageHeaderLength]byte // avoid alloc when reading header

	if err := ch.Conn.SetReadDeadline(time.Now().Add(ch.readTimeout)); err != nil {
		return 0, nil, err
	}

	if _, err := io.ReadFull(ch.br, hrbuf[:]); err != nil {
		return 0, nil, err
	}

	s := binary.BigEndian.Uint32(hrbuf[:4])
	h := binary.BigEndian.Uint32(hrbuf[4:])

	if h > uint32(messageLengthMax) {
		return 0, nil, status.OutOfRange.Err()
	}

	if h > 0 {
		payload := getmbuf(int(h))
		if _, err := io.ReadFull(ch.br, payload); err != nil {
			putmbuf(payload)
			return 0, nil, err
		}
		return s, payload, nil
	}

	return s, nil, nil
}

// discard reads one frame header and skips its payload straight off the
// buffered reader. Unlike Recv it never allocates and never touches the pool,
// which is what makes it cheap to drop a reply for a stream that is already
// gone.
func (ch *channel) discard() (int, error) {
	var hrbuf [messageHeaderLength]byte // avoid alloc when reading header

	if err := ch.Conn.SetReadDeadline(time.Now().Add(ch.readTimeout)); err != nil {
		return 0, err
	}

	if _, err := io.ReadFull(ch.br, hrbuf[:]); err != nil {
		return 0, err
	}

	// the stream id is not needed when the frame is being dropped
	h := binary.BigEndian.Uint32(hrbuf[4:])

	if h > uint32(messageLengthMax) {
		return 0, status.OutOfRange.Err()
	}

	if h > 0 {
		return ch.br.Discard(int(h))
	}

	return 0, nil
}

// Send writes the given byte slices as one or more frames, each prefixed with
// an 8-byte header carrying streamID and that slice's length.
//
// With bufsize <= 0 every call goes straight to the connection. Otherwise the
// frames are appended to a pending write buffer and flushed as soon as it grows
// past bufsize, or by a timer after flushInterval, whichever comes first.
func (ch *channel) Send(streamID uint32, p ...[]byte) error {
	var bw net.Buffers
	for _, b := range p {
		if len(b) > messageLengthMax {
			return status.OutOfRange.Err()
		}
		var hwbuf [messageHeaderLength]byte
		binary.BigEndian.PutUint32(hwbuf[:4], streamID)
		binary.BigEndian.PutUint32(hwbuf[4:], uint32(len(b)))
		bw = append(bw, hwbuf[:], b)
	}
	if ch.bufsize <= 0 {
		if err := ch.Conn.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
			return err
		}
		if _, err := bw.WriteTo(ch.Conn); err != nil {
			return err
		}
		return nil
	}

	ch.Lock()
	defer ch.Unlock()
	var n int
	ch.bw = append(ch.bw, bw...)
	for _, b := range ch.bw {
		n += len(b)
	}

	if n >= ch.bufsize {
		if err := ch.Conn.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
			return err
		}
		if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
			return err
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
		if err := ch.Conn.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
			_ = ch.Conn.Close()
			return
		}
		if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
			_ = ch.Conn.Close()
		}
	})

	return nil
}

// Close stops the flush timer, writes out whatever batching still holds, then
// closes the connection.
func (ch *channel) Close() error {
	defer ch.Conn.Close()
	if ch.t != nil {
		ch.t.Stop()
	}
	if err := ch.Conn.SetWriteDeadline(time.Now().Add(ch.writeTimeout)); err != nil {
		return err
	}
	if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
		return err
	}
	return nil
}

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

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
)

const (
	messageHeaderLength = 8
	// 4MB 与标准 gRPC 默认值对齐
	messageLengthMax = 4 * 1024 * 1024
	// Go 1.27.3 采用的时间轮算法最小时间是1毫秒
	defaultFlushInterval = time.Millisecond
	// bw
	defaultBufSize = 1024
	// linux内核tcp发送缓冲区的默认值是16k
	defaultMaxBufSize = 16 * defaultBufSize
)

// Sender is the interface for sending messages to a channel.
type Sender interface {
	Send(uint32, ...[]byte) error
	io.Closer
}

// messageHeader represents the fixed-length message header of 10 bytes sent
// with every request.
var buffers sync.Pool

// getmbuf returns a buffer from the pool. The buffer is guaranteed to be at least size bytes long.
func getmbuf(size int) []byte {
	// we can't use the standard New method on pool because we want to allocate
	// based on size.
	b, ok := buffers.Get().(*[]byte)
	if !ok || cap(*b) < size {
		// TODO(stevvooe): It may be better to allocate these in fixed length
		// buckets to reduce fragmentation but its not clear that would help
		// with performance. An ilogb approach or similar would work well.
		return make([]byte, size)
	}
	*b = (*b)[:size]
	return *b
}

// putmbuf returns a buffer to the pool. The buffer must have been allocated by getmbuf.
func putmbuf(p []byte) {
	if len(p) == 0 {
		return
	}
	buffers.Put(&p)
}

// channel is a wrapper around a net.Conn that provides methods for sending and receiving messages with a fixed-length header.
type channel struct {
	bufsize  int
	interval time.Duration
	net.Conn
	br *bufio.Reader
	bw net.Buffers
	t  *time.Timer
	sync.Mutex
}

// NewChannel creates a new channel with the given net.Conn.
func newChannel(c net.Conn) *channel {
	x := &channel{
		Conn:     c,
		br:       bufio.NewReader(c),
		interval: defaultFlushInterval,
	}
	return x
}

func newChannelSize(c net.Conn, bufsize int) *channel {
	x := &channel{
		Conn:     c,
		bufsize:  bufsize,
		br:       bufio.NewReader(c),
		interval: defaultFlushInterval,
	}
	return x
}

// recv a message from the channel. The returned buffer contains the message.
//
// If a valid grpc status is returned, the message header
// returned will be valid and caller should send that along to
// the correct consumer. The bytes on the underlying channel
// will be discarded.
func (ch *channel) Recv() (uint32, []byte, error) {
	var hrbuf [messageHeaderLength]byte // avoid alloc when reading header
	_, err := io.ReadFull(ch.br, hrbuf[:])
	if err != nil {
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
			return 0, nil, err
		}
		return s, payload, nil
	}

	return s, nil, nil
}

// Send sends a message to the channel. The message is prefixed with a fixed-length header containing the length of the message and the stream ID.
func (ch *channel) Send(streamID uint32, p ...[]byte) error {
	var bw net.Buffers
	for _, b := range p {
		var hwbuf [messageHeaderLength]byte
		binary.BigEndian.PutUint32(hwbuf[:4], streamID)
		binary.BigEndian.PutUint32(hwbuf[4:], uint32(len(b)))
		bw = append(bw, hwbuf[:], b)
	}

	if ch.bufsize <= 0 {
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
		ch.t.Reset(ch.interval)
		return nil
	}
	ch.t = time.AfterFunc(ch.interval, func() {
		ch.Lock()
		defer ch.Unlock()
		if len(ch.bw) == 0 {
			return
		}
		if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
			_ = ch.Conn.Close()
		}
	})

	return nil
}

func (ch *channel) Close() error {
	ch.Lock()
	defer ch.Unlock()
	defer ch.Conn.Close()
	if ch.t != nil {
		ch.t.Stop()
	}
	if _, err := ch.bw.WriteTo(ch.Conn); err != nil {
		return err
	}
	return nil
}

func recv(channel *channel) (uint32, *api.Header, []byte, error) {
	streamID, h, err := channel.Recv()
	if err != nil {
		return 0, nil, nil, err
	}
	defer putmbuf(h)
	_, p, err := channel.Recv()
	if err != nil {
		return 0, nil, nil, err
	}
	var header api.Header
	if err := encoding.Unmarshal(h, &header); err != nil {
		return 0, nil, nil, err
	}
	return streamID, &header, p, nil

}

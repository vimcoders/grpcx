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
)

const (
	messageHeaderLength  = 8
	messageLengthMax     = 4 * 1024 * 1024 // 4MB 与标准 gRPC 默认值对齐
	defaultFlushInterval = 100 * time.Microsecond
)

// Sender is the interface for sending messages to a channel.
type Sender interface {
	Send(uint32, []byte) error
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
	buffers.Put(&p)
}

// channel is a wrapper around a net.Conn that provides methods for sending and receiving messages with a fixed-length header.
type channel struct {
	net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	push *time.Timer
	sync.Mutex
}

// NewChannel creates a new channel with the given net.Conn.
func newChannel(c net.Conn) *channel {
	x := &channel{
		Conn: c,
		br:   bufio.NewReader(c),
		bw:   bufio.NewWriterSize(c, 16*1024), // 16KB write buffer

	}
	x.push = time.AfterFunc(defaultFlushInterval, func() {
		x.Lock()
		defer x.Unlock()
		_ = x.bw.Flush()
	})
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

	h := binary.BigEndian.Uint32(hrbuf[:4])
	s := binary.BigEndian.Uint32(hrbuf[4:])

	if h > uint32(messageLengthMax) {
		if _, err := ch.br.Discard(int(h)); err != nil {
			return 0, nil, err
		}

		return 0, nil, status.OutOfRange.Err()
	}

	if h > 0 {
		p := getmbuf(int(h))
		if _, err := io.ReadFull(ch.br, p); err != nil {
			return 0, nil, err
		}
		return s, p, nil
	}

	return s, nil, nil
}

// Send sends a message to the channel. The message is prefixed with a fixed-length header containing the length of the message and the stream ID.
func (ch *channel) Send(streamID uint32, p []byte) error {
	if len(p) > messageLengthMax {
		return status.DataLoss.Err()
	}
	hwbuf := getmbuf(messageHeaderLength + len(p))
	defer putmbuf(hwbuf)
	binary.BigEndian.PutUint32(hwbuf[:4], uint32(len(p)))
	binary.BigEndian.PutUint32(hwbuf[4:], streamID)
	copy(hwbuf[messageHeaderLength:], p)
	ch.Lock()
	defer ch.Unlock()
	ch.push.Reset(defaultFlushInterval)
	if _, err := ch.bw.Write(hwbuf); err != nil {
		return err
	}
	return nil
}

func (ch *channel) Close() error {
	defer ch.push.Stop()
	return ch.Conn.Close()
}

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

package ttrpc

import (
	"bufio"
	"encoding/binary"
	"grpcx/status"
	"io"
	"math"
	"net"
	"sync"
)

const (
	messageHeaderLength = 8
	messageLengthMax    = math.MaxUint16
)

// Sender is the interface for sending messages to a channel.
type Sender interface {
	Send(uint32, []byte) error
}

// messageHeader represents the fixed-length message header of 10 bytes sent
// with every request.
var buffers sync.Pool

// channel is a wrapper around a net.Conn that provides methods for sending and receiving messages with a fixed-length header.
type channel struct {
	net.Conn
	br *bufio.Reader
}

// NewChannel creates a new channel with the given net.Conn.
func newChannel(conn net.Conn) *channel {
	return &channel{
		Conn: conn,
		br:   bufio.NewReader(conn),
	}
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
			return 0, nil, status.DataLoss.Err()
		}

		return 0, nil, status.DataLoss.Err()
	}

	var p []byte
	if h > 0 {
		p = ch.getmbuf(int(h))
		if _, err := io.ReadFull(ch.br, p); err != nil {
			return 0, nil, status.DataLoss.Err()
		}
	}

	return s, p, nil
}

// Send sends a message to the channel. The message is prefixed with a fixed-length header containing the length of the message and the stream ID.
func (ch *channel) Send(streamID uint32, p []byte) error {
	if len(p) > messageLengthMax {
		return status.DataLoss.Err()
	}
	hwbuf := ch.getmbuf(messageHeaderLength + len(p))
	defer ch.putmbuf(hwbuf)
	binary.BigEndian.PutUint32(hwbuf[:4], uint32(len(p)))
	binary.BigEndian.PutUint32(hwbuf[4:], streamID)
	copy(hwbuf[8:], p)

	_, err := ch.Write(hwbuf)
	if err != nil {
		return err
	}
	return nil
}

// getmbuf returns a buffer from the pool. The buffer is guaranteed to be at least size bytes long.
func (ch *channel) getmbuf(size int) []byte {
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
func (ch *channel) putmbuf(p []byte) {
	buffers.Put(&p)
}

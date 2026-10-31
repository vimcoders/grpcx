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

	"github.com/vimcoders/grpcx/status"
)

const (
	messageHeaderLength = 8
	messageLengthMax    = 4 * 1024 * 1024 // 4MB 与标准 gRPC 默认值对齐
)

// Sender is the interface for sending messages to a channel.
type Sender interface {
	Send(uint32, []byte) error
}

// channel is a wrapper around a net.Conn that provides methods for sending and receiving messages with a fixed-length header.
type channel struct {
	net.Conn
	br   *bufio.Reader
	bw   *bufio.Writer
	rbuf []byte
	sync.Mutex
}

// NewChannel creates a new channel with the given net.Conn.
func newChannel(c net.Conn) *channel {
	return &channel{
		Conn: c,
		br:   bufio.NewReader(c),
		bw:   bufio.NewWriter(c),
		rbuf: make([]byte, messageLengthMax),
	}
}

// recv a message from the channel. The returned buffer contains the message.
//
// If a valid grpc status is returned, the message header
// returned will be valid and caller should send that along to
// the correct consumer. The bytes on the underlying channel
// will be discarded.
func (ch *channel) Recv() (uint32, []byte, error) {
	rbuf := ch.rbuf[:messageHeaderLength]
	if _, err := io.ReadFull(ch.br, rbuf[:]); err != nil {
		return 0, nil, err
	}

	h := binary.BigEndian.Uint32(rbuf[:4])
	s := binary.BigEndian.Uint32(rbuf[4:])

	if h > uint32(messageLengthMax) {
		if _, err := ch.br.Discard(int(h)); err != nil {
			return 0, nil, err
		}

		return 0, nil, status.OutOfRange.Err()
	}

	if h > 0 {
		p := ch.rbuf[:h]
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
		return status.OutOfRange.Err()
	}
	var bw [messageHeaderLength]byte
	binary.BigEndian.PutUint32(bw[:4], uint32(len(p)))
	binary.BigEndian.PutUint32(bw[4:], streamID)
	ch.Lock()
	defer ch.Unlock()
	defer ch.bw.Flush()
	if _, err := ch.bw.Write(bw[:]); err != nil {
		return err
	}
	if _, err := ch.bw.Write(p); err != nil {
		return err
	}
	return nil
}

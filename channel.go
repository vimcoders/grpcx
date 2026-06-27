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
	"fmt"
	"io"
	"log/slog"
	"net"
	"sync"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

const (
	messageHeaderLength = 4
	messageLengthMax    = 4 << 20
)

// messageHeader represents the fixed-length message header of 10 bytes sent
// with every request.
var buffers sync.Pool

type channel struct {
	conn  net.Conn
	bw    *bufio.Writer
	br    *bufio.Reader
	hrbuf [messageHeaderLength]byte // avoid alloc when reading header
	hwbuf [messageHeaderLength]byte
}

func newChannel(conn net.Conn) *channel {
	return &channel{
		conn: conn,
		bw:   bufio.NewWriter(conn),
		br:   bufio.NewReader(conn),
	}
}

// recv a message from the channel. The returned buffer contains the message.
//
// If a valid grpc status is returned, the message header
// returned will be valid and caller should send that along to
// the correct consumer. The bytes on the underlying channel
// will be discarded.
func (ch *channel) recv() ([]byte, error) {
	_, err := io.ReadFull(ch.br, ch.hrbuf[:])
	if err != nil {
		return nil, err
	}

	mh := binary.BigEndian.Uint32(ch.hrbuf[:4])
	slog.Info("recv", "messageHeader", mh)

	if mh > uint32(messageLengthMax) {
		if _, err := ch.br.Discard(int(mh)); err != nil {
			return nil, fmt.Errorf("failed to discard after receiving oversized message: %w", err)
		}

		return nil, status.Errorf(codes.ResourceExhausted, "message length %v exceed maximum message size of %v", mh, messageLengthMax)
	}

	var p []byte
	if mh > 0 {
		p = ch.getmbuf(int(mh))
		if _, err := io.ReadFull(ch.br, p); err != nil {
			return nil, fmt.Errorf("failed reading message: %w", err)
		}
	}

	return p, nil
}

func (ch *channel) send(streamID uint32, p []byte) error {
	if len(p) > messageLengthMax {
		return OversizedMessageError(len(p))
	}
	binary.BigEndian.PutUint32(ch.hwbuf[:4], uint32(len(p)))
	if _, err := ch.bw.Write(ch.hwbuf[:]); err != nil {
		return err
	}

	if len(p) > 0 {
		_, err := ch.bw.Write(p)
		if err != nil {
			return err
		}
	}

	return ch.bw.Flush()
}

func (ch *channel) getmbuf(size int) []byte {
	// we can't use the standard New method on pool because we want to allocate
	// based on size.
	b, ok := buffers.Get().(*[]byte)
	if !ok || cap(*b) < size {
		// TODO(stevvooe): It may be better to allocate these in fixed length
		// buckets to reduce fragmentation but its not clear that would help
		// with performance. An ilogb approach or similar would work well.
		bb := make([]byte, size)
		b = &bb
	} else {
		*b = (*b)[:size]
	}
	return *b
}

func (ch *channel) putmbuf(p []byte) {
	buffers.Put(&p)
}

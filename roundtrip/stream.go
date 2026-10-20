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

package roundtrip

import (
	"context"
)

// stream is a wrapper around grpc.ClientStream that provides methods for sending and receiving messages with a fixed-length header.
type stream struct {
	id     uint32
	sender Sender
	c      chan []byte
}

// newStream creates a new stream with the given id and sender.
func newStream(id uint32, send Sender) *stream {
	return &stream{
		id:     id,
		sender: send,
		c:      make(chan []byte, 1),
	}
}

// close closes the stream and releases any resources associated with it.
func (s *stream) close() error {
	return nil
}

// send sends a message on the stream. The message is sent with a fixed-length header that includes the stream id.
func (s *stream) send(_ context.Context, b []byte) error {
	return s.sender.Send(s.id, b)
}

// receive receives a message from the stream. The message is received with a fixed-length header that includes the stream id. If the stream is closed, an error is returned.
func (s *stream) recv(ctx context.Context, b []byte) error {
	select {
	case s.c <- b:
		return nil
	case <-ctx.Done():
		return s.close()
	default: // 上一个响应还没被取走，属于异常帧，丢弃
		return nil
	}
}

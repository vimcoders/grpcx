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
	"context"
	"grpcx/codec"
	"grpcx/driver"
	"grpcx/generated/api"
	"sync"
)

type stream struct {
	id     uint32
	sender driver.Sender
	recv   chan *api.Response
	codec.Codec

	closeOnce sync.Once
}

func newStream(id uint32, send driver.Sender) *stream {
	return &stream{
		id:     id,
		sender: send,
		recv:   make(chan *api.Response, 1),
	}
}

func (s *stream) close() error {
	s.closeOnce.Do(func() { close(s.recv) })
	return nil
}

func (s *stream) send(_ context.Context, b []byte) error {
	return s.sender.Send(s.id, b)
}

func (s *stream) receive(ctx context.Context, response *api.Response) error {
	select {
	case s.recv <- response:
		return nil
	case <-ctx.Done():
		return s.close()
	default:
		return s.close()
	}
}

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
	"context"
	"sync"
	"ttrpc/generated/api"
)

type stream struct {
	id     uint32
	sender sender
	recv   chan *api.Response
	codec

	closeOnce sync.Once
	recvErr   error
	recvClose chan struct{}
}

func newStream(id uint32, send sender) *stream {
	return &stream{
		id:        id,
		sender:    send,
		recv:      make(chan *api.Response, 1),
		recvClose: make(chan struct{}),
	}
}

func (s *stream) closeWithError(err error) error {
	s.closeOnce.Do(func() {
		if err != nil {
			s.recvErr = err
		}
		close(s.recvClose)
	})
	return nil
}

func (s *stream) send(ctx context.Context, b []byte) error {
	return s.sender.send(s.id, b)
}

func (s *stream) receive(ctx context.Context, response *api.Response) error {
	select {
	case <-s.recvClose:
		return s.recvErr
	case s.recv <- response:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

type sender interface {
	send(uint32, []byte) error
}

package grpcx

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"time"

	"github.com/vimcoders/go-driver/log"

	"google.golang.org/protobuf/proto"
)

type sender struct {
	Option
	net.Conn
	seq     uint32
	timeout time.Duration
	signal  chan Message
}

func (x *sender) invoke(iMessage Message) error {
	defer func() {
		if err := recover(); err != nil {
			log.Error(err)
		}
	}()
	x.signal <- iMessage
	return nil
}

func (x *sender) push(ctx context.Context, methodName string, req proto.Message) (Message, error) {
	for method := 0; method < len(x.Methods); method++ {
		if x.Methods[method].MethodName != filepath.Base(methodName) {
			continue
		}
		buf, err := encode(x.seq, uint16(method), req)
		if err != nil {
			return nil, err
		}
		if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
			return nil, err
		}
		if _, err := buf.WriteTo(x.Conn); err != nil {
			return nil, err
		}
		select {
		case iMessage := <-x.signal:
			return iMessage, nil
		case <-ctx.Done():
			close(x.signal)
			return nil, errors.New("timeout")
		}
	}
	return nil, errors.New(methodName)
}

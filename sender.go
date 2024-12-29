package grpcx

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type sender struct {
	net.Conn
	seq     uint16
	timeout time.Duration
	signal  chan Message
	Methods []grpc.MethodDesc
	encode  func(seq uint16, method uint16, iMessage proto.Message) (Message, error)
}

func (x *sender) invoke(iMessage Message) error {
	defer func() {
		if err := recover(); err != nil {
			debug.PrintStack()
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
		buf, err := x.encode(x.seq, uint16(method), req)
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

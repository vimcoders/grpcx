package grpcx

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"runtime/debug"
	"time"

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
		buf, err := x.encode(ctx, x.seq, uint16(method), req)
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

func (x *sender) encode(ctx context.Context, seq uint32, method uint16, iMessage proto.Message) (Message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*Message)
	if opentracing, ok := ctx.Value("opentracing").(OpenTracing); ok {
		buf.WriteUint16(uint16(32 + len(b))) // 2
		buf.WriteUint32(seq)                 // 4
		buf.WriteUint16(method)              // 2
		buf.WriteUint64(opentracing.High)    // 8
		buf.WriteUint64(opentracing.Low)     // 8
		buf.WriteUint64(opentracing.SpanID)  // 8
		if _, err := buf.Write(b); err != nil {
			return nil, err
		}
		return *buf, nil
	}
	buf.WriteUint16(uint16(8 + len(b))) // 2
	buf.WriteUint32(seq)                // 4
	buf.WriteUint16(method)             // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

package grpcx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/log"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type conn struct {
	net.Conn
	clientOption
	grpc.ClientConnInterface
	q   []chan []byte
	seq chan uint16
	context.Context
	sync.RWMutex
}

func (x *conn) Close() error {
	return x.Conn.Close()
}

func (x *conn) Network() string {
	return x.LocalAddr().Network()
}

func (x *conn) String() string {
	return x.LocalAddr().String()
}

func (x *conn) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) (err error) {
	for i := 0; i < len(x.Methods); i++ {
		if x.Methods[i] != filepath.Base(method) {
			continue
		}
		return x.invoke(ctx, uint16(i), req, reply)
	}
	return fmt.Errorf("method %s not found", method)
}

func (x *conn) invoke(ctx context.Context, cmd uint16, req any, reply any) error {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return err
	}
	for i := 1; i <= x.maxRetry; i++ {
		response, err := x.do(ctx, cmd, b)
		if err != nil {
			log.Error(err)
			continue
		}
		if err := proto.Unmarshal(response, reply.(proto.Message)); err != nil {
			return err
		}
		return nil
	}
	return errors.New("faild")
}

func (x *conn) do(ctx context.Context, cmd uint16, b []byte) ([]byte, error) {
	var seq uint16
	select {
	case <-x.Done():
		return nil, errors.New("shutdown")
	case <-ctx.Done():
		return nil, errors.New("timeout")
	case seq = <-x.seq:
		for i := 0; i < len(x.q[seq]); i++ {
			<-x.q[seq]
		}
		if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
			x.seq <- seq
			return nil, err
		}
		req := request{cmd: cmd, seq: seq, b: b}
		if _, err := req.WriteTo(x.Conn); err != nil {
			x.seq <- seq
			return nil, err
		}
	}
	select {
	case <-x.Done():
		return nil, errors.New("shutdown")
	case <-ctx.Done():
		x.seq <- seq
		return nil, errors.New("timeout")
	case response := <-x.q[seq]:
		x.seq <- seq
		return response, nil
	}
}

func (x *conn) serve(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
		if err := x.Close(); err != nil {
			log.Error(err)
		}
	}()
	buf := bufio.NewReaderSize(x.Conn, int(x.buffsize))
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
			if err := x.Conn.SetReadDeadline(time.Now().Add(x.timeout)); err != nil {
				return err
			}
			response, err := readResponse(buf)
			if err != nil {
				return err
			}
			x.q[response.seq] <- response.b
		}
	}
}

func (x *conn) Ping(ctx context.Context) error {
	response, err := x.do(ctx, math.MaxUint16, nil)
	if err != nil {
		return err
	}
	if len(x.Methods) > 0 {
		return nil
	}
	var methods []string
	if err := json.Unmarshal(response, &methods); err != nil {
		return err
	}
	x.Methods = methods
	return nil
}

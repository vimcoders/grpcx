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
	q  []chan *response
	ch chan uint16
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
	for i := 1; i <= x.maxRetry; i++ {
		ch, err := x.do(ctx, cmd, req)
		if err != nil {
			log.Error(err)
			continue
		}
		select {
		case <-x.Done():
			return errors.New("shutdown")
		case <-ctx.Done():
			return errors.New("timeout")
		case response := <-ch:
			x.ch <- response.seq
			if err := proto.Unmarshal(response.b, reply.(proto.Message)); err != nil {
				return err
			}
			return nil
		}
	}
	return errors.New("faild")
}

func (x *conn) do(ctx context.Context, cmd uint16, req any) (<-chan *response, error) {
	select {
	case seq := <-x.ch:
		for i := 0; i < len(x.q[seq]); i++ {
			<-x.q[seq]
		}
		request := request{cmd: cmd, seq: seq}
		if req != nil {
			b, err := proto.Marshal(req.(proto.Message))
			if err != nil {
				x.ch <- seq
				return nil, err
			}
			request.b = b
		}
		if err := x.push(ctx, &request); err != nil {
			x.ch <- seq
			return nil, err
		}
		return x.q[seq], nil
	case <-x.Done():
		return nil, errors.New("shutdown")
	case <-ctx.Done():
		return nil, errors.New("timeout")
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
			x.q[response.seq] <- &response
		}
	}
}

func (x *conn) push(_ context.Context, req *request) error {
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	if _, err := req.WriteTo(x.Conn); err != nil {
		return err
	}
	return nil
}

func (x *conn) Ping(ctx context.Context) error {
	ch, err := x.do(ctx, math.MaxUint16, nil)
	if err != nil {
		return err
	}
	select {
	case <-x.Done():
		return errors.New("shutdown")
	case <-ctx.Done():
		return errors.New("timeout")
	case response := <-ch:
		if len(x.Methods) > 0 {
			return nil
		}
		var methods []string
		if err := json.Unmarshal(response.b, &methods); err != nil {
			return err
		}
		x.Methods = methods
		return nil
	}
}

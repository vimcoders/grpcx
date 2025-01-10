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
	pending map[uint16]request
	ch      chan request
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
	request, err := x.NewRequest(ctx, cmd, req)
	if err != nil {
		return err
	}
	for i := 1; i <= x.maxRetry; i++ {
		response, err := x.do(ctx, request)
		if err != nil {
			log.Error(err)
			continue
		}
		x.ch <- request
		if err := proto.Unmarshal(response.Bytes(), reply.(proto.Message)); err != nil {
			return err
		}
		return nil
	}
	return errors.New("faild")
}

func (x *conn) NewRequest(ctx context.Context, cmd uint16, req any) (request, error) {
	select {
	case v := <-x.ch:
		if req == nil {
			v.b = nil
			v.cmd = cmd
			return v, nil
		}
		b, err := proto.Marshal(req.(proto.Message))
		if err != nil {
			return request{}, err
		}
		v.b = b
		v.cmd = cmd
		return v, nil
	case <-x.Done():
		return request{}, errors.New("shutdown")
	case <-ctx.Done():
		return request{}, errors.New("timeout")
	}
}

func (x *conn) do(ctx context.Context, req request) (buffer, error) {
	err := x.push(ctx, req)
	if err != nil {
		return buffer{}, err
	}
	select {
	case <-ctx.Done():
		return buffer{}, errors.New("timeout")
	case <-x.Done():
		return buffer{}, errors.New("shutdown")
	case b := <-req.ch:
		return b, nil
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
			x.take(response)
		}
	}
}

func (x *conn) take(response response) error {
	x.Lock()
	defer x.Unlock()
	if req, ok := x.pending[response.seq]; ok {
		delete(x.pending, response.seq)
		req.invoke(response.b)
	}
	return nil
}

func (x *conn) push(_ context.Context, req request) error {
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	x.Lock()
	defer x.Unlock()
	if _, err := req.WriteTo(x.Conn); err != nil {
		return err
	}
	x.pending[req.seq] = req
	return nil
}

func (x *conn) Ping(ctx context.Context) error {
	request, err := x.NewRequest(ctx, math.MaxUint16, nil)
	if err != nil {
		return err
	}
	b, err := x.do(ctx, request)
	if err != nil {
		return err
	}
	x.ch <- request
	if len(x.Methods) > 0 {
		return nil
	}
	var methods []string
	if err := json.Unmarshal(b.Bytes(), &methods); err != nil {
		return err
	}
	x.Methods = methods
	return nil
}

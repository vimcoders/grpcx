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
	seq     uint16
	context.Context
	context.CancelFunc
	sync.Pool
	sync.RWMutex
}

func (x *conn) Close() error {
	x.CancelFunc()
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

func (x *conn) invoke(ctx context.Context, method uint16, req any, reply any) error {
	for i := 1; i <= x.maxRetry; i++ {
		request, err := x.NewGRPCXRequest(method, req)
		if err != nil {
			return err
		}
		defer x.Pool.Put(&request)
		b, err := x.do(ctx, request)
		if err != nil {
			time.Sleep(x.retrySleep * time.Duration(i))
			log.Error(err)
			continue
		}
		if err := proto.Unmarshal(b.Bytes(), reply.(proto.Message)); err != nil {
			return err
		}
		return nil
	}
	return nil
}

func (x *conn) NewGRPCXRequest(cmd uint16, req any) (request, error) {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return request{}, err
	}
	request := x.Pool.Get().(*request)
	request.cmd = cmd
	request.b = b
	return *request, nil
}

func (x *conn) NewPingRequest() request {
	request := x.Pool.Get().(*request)
	request.cmd = math.MaxUint16
	return *request
}

func (x *conn) NewRequest() any {
	x.Lock()
	defer x.Unlock()
	seq := x.seq + 1
	x.seq = seq % math.MaxUint16
	return &request{seq: seq, ch: make(chan buffer, 1)}
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

func (x *conn) keepalive(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
		x.Close()
	}()
	ticker := time.NewTicker(x.KeepaliveParams.Time)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		case <-ticker.C:
			if err := x.Ping(ctx); err != nil {
				return err
			}
		}
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
	go x.keepalive(ctx)
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
	x.Lock()
	defer x.Unlock()
	if _, ok := x.pending[req.seq]; ok {
		return errors.New("too many request")
	}
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	if _, err := req.WriteTo(x.Conn); err != nil {
		return err
	}
	x.pending[req.seq] = req
	return nil
}

func (x *conn) Ping(ctx context.Context) error {
	request := x.NewPingRequest()
	defer x.Pool.Put(&request)
	b, err := x.do(ctx, request)
	if err != nil {
		return err
	}
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

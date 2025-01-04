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

	"github.com/vimcoders/grpcx/balance"
	"github.com/vimcoders/grpcx/discovery"
	"github.com/vimcoders/grpcx/log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

type clientOption struct {
	buffsize        uint16
	timeout         time.Duration
	Methods         []string
	maxRetry        int
	retrySleep      time.Duration
	KeepaliveParams keepalive.ClientParameters
}

type client struct {
	cc []*conn
	grpc.ClientConnInterface
	balance.Balancer
	discovery.Result
}

func (x *client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) (err error) {
	if len(x.Instances) <= 0 {
		return fmt.Errorf("instance %s not found", method)
	}
	instance := x.GetPicker(x.Result).Next(ctx, req)
	for i := 0; i < len(x.cc); i++ {
		if x.cc[i].Network() != instance.Address().Network() {
			continue
		}
		if x.cc[i].String() != instance.Address().String() {
			continue
		}
		return x.cc[i].Invoke(ctx, method, req, reply, opts...)
	}
	return fmt.Errorf("instance %s not found", instance.Address().String())
}

func (x *client) GetPicker(result discovery.Result) balance.Picker {
	return balance.NewRandomPicker(result.Instances)
}

type conn struct {
	sync.RWMutex
	net.Conn
	clientOption
	grpc.ClientConnInterface
	pending map[uint16]chan *buffer
	seq     uint16
	context.Context
	context.CancelFunc
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
		request, err := NewRequest(method, req)
		if err != nil {
			return err
		}
		b, err := x.do(ctx, request)
		if err != nil {
			time.Sleep(x.retrySleep * time.Duration(i))
			log.Error(err)
			continue
		}
		defer b.Close()
		if err := proto.Unmarshal(b.Bytes(), reply.(proto.Message)); err != nil {
			return err
		}
		return nil
	}
	return nil
}

func (x *conn) do(ctx context.Context, req *request) (*buffer, error) {
	ch, err := x.push(ctx, req)
	if err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, errors.New("timeout")
	case <-x.Done():
		return nil, errors.New("shutdown")
	case b := <-ch:
		if b == nil {
			return nil, errors.New("too many request")
		}
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
		x.Close()
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
			x.process(response)
		}
	}
}

func (x *conn) process(response *response) error {
	x.Lock()
	defer x.Unlock()
	if v, ok := x.pending[response.seq]; ok && v != nil {
		delete(x.pending, response.seq)
		if len(v) > 0 {
			return nil
		}
		v <- response.Clone()
	}
	return nil
}

func (x *conn) push(_ context.Context, req *request) (<-chan *buffer, error) {
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return nil, err
	}
	x.Lock()
	defer x.Unlock()
	req.seq = x.seq + 1
	if _, ok := x.pending[req.seq]; ok {
		return nil, errors.New("too many request")
	}
	ch := make(chan *buffer, 1)
	x.pending[req.seq] = ch
	if _, err := req.WriteTo(x.Conn); err != nil {
		delete(x.pending, req.seq)
		return nil, err
	}
	x.seq = req.seq % math.MaxUint16
	return ch, nil
}

func (x *conn) Ping(ctx context.Context) error {
	b, err := x.do(ctx, NewPingRequest())
	if err != nil {
		return err
	}
	defer b.Close()
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

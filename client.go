package grpcx

import (
	"bufio"
	"context"
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

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
	"google.golang.org/protobuf/proto"
)

type Conn interface {
	net.Addr
	grpc.ClientConnInterface
}

type clientOption struct {
	buffsize        uint16
	timeout         time.Duration
	Methods         []string
	maxRetry        int
	retrySleep      time.Duration
	KeepaliveParams keepalive.ClientParameters
}

type client struct {
	cc []Conn
	grpc.ClientConnInterface
	balance.Balancer
	discovery.Result
}

func (x *client) Invoke(ctx context.Context, methodName string, req any, reply any, opts ...grpc.CallOption) (err error) {
	instance := x.GetPicker(x.Result).Next(ctx, req)
	for i := 0; i < len(x.cc); i++ {
		if i != instance.Weight() {
			continue
		}
		// if x.cc[i].Network() != instance.Address().Network() {
		// 	continue
		// }
		// if x.cc[i].String() != instance.Address().String() {
		// 	continue
		// }
		return x.cc[i].Invoke(ctx, methodName, req, reply, opts...)
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
	pending map[uint16]*request
	seq     uint16
}

func newClient(ctx context.Context, c net.Conn, opt clientOption) Conn {
	x := &conn{
		clientOption: opt,
		Conn:         c,
		pending:      make(map[uint16]*request),
		seq:          math.MaxUint8,
	}
	go x.serve(ctx)
	return x
}

func (x *conn) Close() error {
	return x.Conn.Close()
}

func (x *conn) Network() string {
	return x.RemoteAddr().Network()
}

func (x *conn) String() string {
	return x.RemoteAddr().String()
}

func (x *conn) Invoke(ctx context.Context, methodName string, req any, reply any, opts ...grpc.CallOption) (err error) {
	for method := 0; method < len(x.Methods); method++ {
		if x.Methods[method] != filepath.Base(methodName) {
			continue
		}
		return x.invoke(ctx, uint16(method), req, reply)
	}
	return fmt.Errorf("method %s not found", methodName)
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
			fmt.Println(err)
			continue
		}
		if err := proto.Unmarshal(b.body(), reply.(proto.Message)); err != nil {
			return err
		}
		b.close()
		return nil
	}
	return nil
}

func (x *conn) do(ctx context.Context, req *request) (b *buffer, err error) {
	if err := x.push(req); err != nil {
		return nil, err
	}
	select {
	case <-ctx.Done():
		return nil, errors.New("timeout")
	case b := <-req.ch:
		if b == nil {
			return nil, errors.New("too many request")
		}
		return b, nil
	}
}

func (x *conn) keepalive(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
			debug.PrintStack()
		}
		if err != nil {
			fmt.Println(err)
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
			if err := x.ping(ctx); err != nil {
				return err
			}
		}
	}
}
func (x *conn) serve(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
			debug.PrintStack()
		}
		if err != nil {
			fmt.Println(err)
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
			buffer, err := readBuffer(buf)
			if err != nil {
				return err
			}
			x.process(buffer)
		}
	}
}

func (x *conn) process(buffer *buffer) error {
	if int(buffer.cmd()) >= len(x.Methods) {
		return nil
	}
	seq := buffer.seq()
	x.Lock()
	defer x.Unlock()
	if v, ok := x.pending[seq]; ok && v != nil {
		v.invoke(buffer)
		delete(x.pending, seq)
	}
	return nil
}

func (x *conn) push(req *request) error {
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	x.Lock()
	defer x.Unlock()
	seq := x.seq + 1
	if _, ok := x.pending[seq]; ok {
		close(req.ch)
		return nil
	}
	buf := buffers.Get().(*buffer)
	buf.WriteUint16(req.Size(), seq, req.cmd)
	if _, err := buf.Write(req.body); err != nil {
		return err
	}
	x.pending[seq] = req
	x.seq = seq % math.MaxUint16
	if _, err := buf.WriteTo(x); err != nil {
		return err
	}
	return nil
}

func (x *conn) ping(_ context.Context) error {
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	buf := buffers.Get().(*buffer)
	buf.WriteUint16(6, math.MaxUint16, math.MaxUint16)
	if _, err := buf.WriteTo(x); err != nil {
		return err
	}
	return nil
}

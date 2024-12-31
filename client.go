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
	"time"

	"github.com/vimcoders/grpcx/balance"
	"github.com/vimcoders/grpcx/discovery"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type Conn interface {
	net.Addr
	grpc.ClientConnInterface
}

type clientOption struct {
	buffsize uint16
	timeout  time.Duration
	Methods  []string
	maxRetry int
	ttl      time.Duration
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
	net.Conn
	clientOption
	grpc.ClientConnInterface
	pending map[uint16]*request
	seq     uint16
	sendQ   chan *request
	readQ   chan *buffer
}

func newClient(ctx context.Context, c net.Conn, opt clientOption) Conn {
	x := &conn{
		clientOption: opt,
		Conn:         c,
		pending:      make(map[uint16]*request),
		sendQ:        make(chan *request, 65535),
		readQ:        make(chan *buffer, 65535),
	}
	go x.serve(ctx)
	return x
}

func (x *conn) Close() error {
	close(x.sendQ)
	close(x.readQ)
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
		err := x.do(ctx, method, req, reply)
		if err == nil {
			return nil
		}
		if i == x.maxRetry {
			return err
		}
	}
	return nil
}

func (x *conn) do(ctx context.Context, method uint16, req any, reply any) (err error) {
	request, err := x.newRequest(method, req)
	if err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		return errors.New("timeout")
	case x.sendQ <- request:
	}
	select {
	case <-ctx.Done():
		return errors.New("timeout")
	case b := <-request.ch:
		if b == nil {
			return errors.New("too many request")
		}
		if err := proto.Unmarshal(b.body(), reply.(proto.Message)); err != nil {
			return err
		}
		b.close()
		return nil
	}
}

func (x *conn) read(ctx context.Context) (err error) {
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
			x.readQ <- buffer
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
	}()
	go x.read(ctx)
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		case req := <-x.sendQ:
			err := x.push(req)
			if err != nil {
				close(req.ch)
				return err
			}
		case b := <-x.readQ:
			seq := b.seq()
			if v, ok := x.pending[seq]; ok {
				delete(x.pending, seq)
				v.ch <- b
			}
		}
	}
}

func (x *conn) newRequest(method uint16, req any) (*request, error) {
	b, err := proto.Marshal(req.(proto.Message))
	if err != nil {
		return nil, err
	}
	return &request{
		method: method,
		body:   b,
		ch:     make(chan *buffer, 1),
		now:    time.Now(),
	}, nil
}

func (x *conn) push(req *request) error {
	seq := x.seq + 1
	if v, ok := x.pending[seq]; ok && time.Since(v.now) < x.ttl {
		return errors.New("too many request")
	}
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	req.seq = seq
	if _, err := req.WriteTo(x); err != nil {
		return err
	}
	x.pending[seq] = req
	x.seq = req.seq % math.MaxUint16
	return nil
}

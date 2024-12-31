package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"grpcx/balance"
	"grpcx/discovery"
	"math"
	"net"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

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
	sync.RWMutex
	grpc.ClientConnInterface
	pending map[uint16]*signal
	seq     uint16
}

func newClient(ctx context.Context, c net.Conn, opt clientOption) Conn {
	x := &conn{
		clientOption: opt,
		Conn:         c,
		pending:      make(map[uint16]*signal),
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
		return x.do(ctx, uint16(method), req, reply)
	}
	return fmt.Errorf("method %s not found", methodName)
}

func (x *conn) do(ctx context.Context, method uint16, req any, reply any) (err error) {
	seq, signal, ok := x.newSignal()
	if !ok {
		return fmt.Errorf("too many request")
	}
	buf, err := x.encode(seq, uint16(method), req.(proto.Message))
	if err != nil {
		return err
	}
	if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
		return err
	}
	if _, err := buf.WriteTo(x.Conn); err != nil {
		return err
	}
	select {
	case <-ctx.Done():
		x.notify(seq)
		return errors.New("timeout")
	case response := <-signal.signal:
		if err := proto.Unmarshal(response.body(), reply.(proto.Message)); err != nil {
			return err
		}
		response.close()
		_signal.Put(signal)
		return nil
	}
}

func (x *conn) serve(ctx context.Context) (err error) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
			debug.PrintStack()
		}
	}()
	buf := bufio.NewReaderSize(x.Conn, int(x.buffsize))
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
		}
		if err := x.Conn.SetReadDeadline(time.Now().Add(x.timeout)); err != nil {
			return err
		}
		iMessage, err := x.decode(buf)
		if err != nil {
			return err
		}
		invoker := x.notify(iMessage.seq())
		if invoker == nil {
			return nil
		}
		if err := invoker.invoke(iMessage.clone()); err != nil {
			return err
		}
	}
}

func (x *conn) newSignal() (uint16, *signal, bool) {
	x.Lock()
	defer x.Unlock()
	seq := x.seq + 1
	if _, ok := x.pending[seq]; ok {
		return 0, nil, false
	}
	signal := _signal.Get().(*signal)
	x.pending[seq] = signal
	x.seq = seq % math.MaxUint16
	return seq, signal, true
}

func (x *conn) notify(seq uint16) *signal {
	x.Lock()
	defer x.Unlock()
	if v, ok := x.pending[seq]; ok {
		delete(x.pending, seq)
		return v
	}
	return nil
}

func (x *conn) decode(b *bufio.Reader) (message, error) {
	headerBytes, err := b.Peek(_MESSAGE_HEADER)
	if err != nil {
		return nil, err
	}
	length := int(binary.BigEndian.Uint16(headerBytes))
	if length > b.Size() {
		return nil, fmt.Errorf("header %v too long", length)
	}
	iMessage, err := b.Peek(length)
	if err != nil {
		return nil, err
	}
	if _, err := b.Discard(len(iMessage)); err != nil {
		return nil, err
	}
	return iMessage, nil
}

func (x *conn) encode(seq uint16, method uint16, iMessage proto.Message) (message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*message)
	buf.WriteUint16(uint16(_MESSAGE_HEADER_LENGTH + len(b))) // 2
	buf.WriteUint16(seq)                                     // 2
	buf.WriteUint16(method)                                  // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

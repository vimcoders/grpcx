package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"runtime/debug"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type Option struct {
	buffsize uint16
	timeout  time.Duration
	Methods  []grpc.MethodDesc
}

var defaultClientOptions = Option{
	timeout:  120 * time.Second,
	buffsize: defaultReadBufSize,
}

type XClient struct {
	Option
	net.Conn
	grpc.ClientConnInterface
	sync.RWMutex
	pending map[uint16]*invoker
}

func newClient(ctx context.Context, c net.Conn, opt Option) grpc.ClientConnInterface {
	x := &XClient{
		Option:  opt,
		Conn:    c,
		pending: make(map[uint16]*invoker),
	}
	go x.serve(ctx)
	return x
}

func (x *XClient) Close() error {
	return x.Conn.Close()
}

func (x *XClient) Invoke(ctx context.Context, methodName string, req any, reply any, opts ...grpc.CallOption) (err error) {
	for method := 0; method < len(x.Methods); method++ {
		if x.Methods[method].MethodName != filepath.Base(methodName) {
			continue
		}
		return x.do(ctx, uint16(method), req, reply)
	}
	return errors.New(methodName)
}

func (x *XClient) do(ctx context.Context, method uint16, req any, reply any) (err error) {
	invoker := invoke.Get().(*invoker)
	if ok := x.wait(invoker); !ok {
		return errors.New("too many request")
	}
	buf, err := x.encode(invoker.seq, uint16(method), req.(proto.Message))
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
		x.invoke(invoker.seq)
		return errors.New("timeout")
	case response := <-invoker.signal:
		if err := proto.Unmarshal(response.body(), reply.(proto.Message)); err != nil {
			return err
		}
		response.close()
		invoke.Put(invoker)
		return nil
	}
}

func (x *XClient) serve(ctx context.Context) (err error) {
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
		invoker := x.invoke(iMessage.seq())
		if invoker == nil {
			return nil
		}
		if err := invoker.invoke(iMessage.clone()); err != nil {
			return err
		}
	}
}

func (x *XClient) wait(s *invoker) bool {
	x.Lock()
	defer x.Unlock()
	if _, ok := x.pending[s.seq]; ok {
		return false
	}
	x.pending[s.seq] = s
	return true
}

func (x *XClient) invoke(seq uint16) *invoker {
	x.Lock()
	defer x.Unlock()
	if v, ok := x.pending[seq]; ok {
		delete(x.pending, seq)
		return v
	}
	return nil
}

func (x *XClient) decode(b *bufio.Reader) (Message, error) {
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

func (x *XClient) encode(seq uint16, method uint16, iMessage proto.Message) (Message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*Message)
	buf.WriteUint16(uint16(_MESSAGE_HEADER_LENGTH + len(b))) // 2
	buf.WriteUint16(seq)                                     // 2
	buf.WriteUint16(method)                                  // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

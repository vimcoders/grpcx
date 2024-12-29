package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type Client interface {
	grpc.ClientConnInterface
	Close() error
}

type Option struct {
	buffsize uint16
	timeout  time.Duration
	Methods  []grpc.MethodDesc
}

type XClient struct {
	Option
	net.Conn
	grpc.ClientConnInterface
	sync.RWMutex
	seq     uint32
	pending map[uint32]*sender
	streams *sync.Pool
}

func NewClient(c net.Conn, opt Option) Client {
	return newClient(c, opt)
}

func newClient(c net.Conn, opt Option) Client {
	opt.buffsize = 8 * 1024
	opt.timeout = time.Second * 60
	x := &XClient{
		Option:  opt,
		Conn:    c,
		pending: make(map[uint32]*sender),
	}
	x.streams = &sync.Pool{
		New: func() any {
			seq := x.seq + 1
			x.seq = seq % math.MaxUint32
			return &sender{
				seq:     seq,
				Conn:    x.Conn,
				signal:  make(chan Message, 1),
				timeout: opt.timeout,
				Methods: opt.Methods,
				encode:  x.encode,
			}
		},
	}
	go x.serve(context.Background())
	return x
}

func (x *XClient) Close() error {
	return x.Conn.Close()
}

func (x *XClient) Invoke(ctx context.Context, methodName string, req any, reply any, opts ...grpc.CallOption) (err error) {
	sender := x.streams.Get().(*sender)
	x.wait(sender)
	response, err := sender.push(ctx, methodName, req.(proto.Message))
	if err != nil {
		x.done(sender.seq)
		return err
	}
	if err := proto.Unmarshal(response[8:], reply.(proto.Message)); err != nil {
		return err
	}
	response.reset()
	x.streams.Put(sender)
	return nil
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
		ch := x.done(iMessage.seq())
		if ch == nil {
			return nil
		}
		if err := ch.invoke(iMessage.clone()); err != nil {
			return err
		}
	}
}

func (x *XClient) wait(s *sender) {
	x.Lock()
	defer x.Unlock()
	x.pending[s.seq] = s
}

func (x *XClient) done(seq uint32) *sender {
	x.Lock()
	defer x.Unlock()
	if v, ok := x.pending[seq]; ok {
		delete(x.pending, seq)
		return v
	}
	return nil
}

func (x *XClient) decode(b *bufio.Reader) (Message, error) {
	headerBytes, err := b.Peek(2)
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

func (x *XClient) encode(seq uint32, method uint16, iMessage proto.Message) (Message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*Message)
	buf.WriteUint16(uint16(8 + len(b))) // 2
	buf.WriteUint32(seq)                // 4
	buf.WriteUint16(method)             // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

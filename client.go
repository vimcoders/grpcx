package grpcx

import (
	"bufio"
	"context"
	"errors"
	"math"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"github.com/vimcoders/go-driver/log"

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
				Option:  opt,
				seq:     seq,
				Conn:    x.Conn,
				signal:  make(chan Message, 1),
				timeout: x.timeout,
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
	if err := proto.Unmarshal(response.payload(), reply.(proto.Message)); err != nil {
		return err
	}
	response.reset()
	x.streams.Put(sender)
	return nil
}

func (x *XClient) serve(ctx context.Context) (err error) {
	defer func() {
		if err := recover(); err != nil {
			log.Error(err)
		}
		if err != nil {
			log.Error(err.Error())
		}
		if err := x.Close(); err != nil {
			log.Error(err.Error())
		}
		debug.PrintStack()
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
		iMessage, err := decode(buf)
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

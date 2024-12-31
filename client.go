package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
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
	streams map[uint16]*stream
	seq     uint16
	queue   chan message
}

func newClient(ctx context.Context, c net.Conn, opt clientOption) Conn {
	x := &conn{
		clientOption: opt,
		Conn:         c,
		streams:      make(map[uint16]*stream),
		queue:        make(chan message, 65535),
	}
	go x.serve(ctx)
	return x
}

func (x *conn) Close() error {
	close(x.queue)
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
	stream, ok := x.newStream()
	if !ok {
		return fmt.Errorf("too many request")
	}
	defer func() {
		if err != nil {
			x.closeStream(stream.seq)
		}
	}()
	buf, err := x.encode(stream.seq, uint16(method), req.(proto.Message))
	if err != nil {
		return err
	}
	x.queue <- buf
	select {
	case <-ctx.Done():
		return errors.New("timeout")
	case response := <-stream.signal:
		if err := proto.Unmarshal(response.body(), reply.(proto.Message)); err != nil {
			return err
		}
		response.close()
		streams.Put(stream)
		return nil
	}
}

func (x *conn) send(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
			debug.PrintStack()
		}
		if err != nil {
			fmt.Println(err)
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		case buf := <-x.queue:
			if err := x.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
				return err
			}
			if _, err := buf.WriteTo(x.Conn); err != nil {
				return err
			}
		}
	}
}

func (x *conn) serve(ctx context.Context) (err error) {
	defer func() {
		if err := recover(); err != nil {
			fmt.Println(err)
			debug.PrintStack()
		}
		x.Close()
	}()
	go x.send(ctx)
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
		stream := x.closeStream(iMessage.seq())
		if stream == nil {
			return nil
		}
		if err := stream.invoke(iMessage.clone()); err != nil {
			return err
		}
	}
}

func (x *conn) newStream() (*stream, bool) {
	x.Lock()
	defer x.Unlock()
	seq := x.seq + 1
	if _, ok := x.streams[seq]; ok {
		return nil, false
	}
	stream := streams.Get().(*stream)
	x.streams[seq] = stream
	x.seq = seq % math.MaxUint16
	stream.seq = seq
	return stream, true
}

func (x *conn) closeStream(seq uint16) *stream {
	x.Lock()
	defer x.Unlock()
	if v, ok := x.streams[seq]; ok {
		delete(x.streams, seq)
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

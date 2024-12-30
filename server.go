package grpcx

import (
	"bufio"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"runtime/debug"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

type UnaryServerInterceptor = grpc.UnaryServerInterceptor

type ServerOption interface {
	apply(*serverOption)
}

type funcServerOption struct {
	f func(*serverOption)
}

func (x *funcServerOption) apply(o *serverOption) {
	x.f(o)
}

func newFuncServerOption(f func(*serverOption)) ServerOption {
	return &funcServerOption{
		f: f,
	}
}

func WithServiceDesc(info grpc.ServiceDesc) ServerOption {
	return newFuncServerOption(func(o *serverOption) {
		o.Methods = info.Methods
	})
}

func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOption) {
		o.Unary = i
	})
}

const (
	//defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	//defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultReadBufSize = 32 * 1024
)

type Handler interface {
}

// ListenAndServe binds port and handle requests, blocking until close
func (x Server) ListenAndServe(ctx context.Context, listener net.Listener) {
	defer func() {
		if err := recover(); err != nil {
			debug.PrintStack()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := listener.Accept()
		if err != nil {
			fmt.Println(err)
			continue
		}
		go x.serve(ctx, conn)
	}
}

type serverOption struct {
	// creds             credentials.TransportCredentials
	readBufferSize    int
	connectionTimeout time.Duration
	Unary             UnaryServerInterceptor
	Methods           []grpc.MethodDesc
}

var defaultServerOptions = serverOption{
	connectionTimeout: 120 * time.Second,
	readBufferSize:    defaultReadBufSize,
}

type Server struct {
	serverOption
	impl any
}

func NewServer(impl any, opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for i := 0; i < len(opt); i++ {
		opt[i].apply(&opts)
	}
	return &Server{impl: impl, serverOption: opts}
}

func (x *Server) Close() error {
	return nil
}

func (x *Server) serve(ctx context.Context, c net.Conn) (err error) {
	defer func() {
		if e := recover(); e != nil {
			fmt.Println(e)
		}
		if err != nil {
			fmt.Println(err)
		}
		if err := x.Close(); err != nil {
			fmt.Println(err)
		}
		debug.PrintStack()
	}()
	buf := bufio.NewReaderSize(c, x.readBufferSize)
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
		}
		if err := c.SetReadDeadline(time.Now().Add(x.connectionTimeout)); err != nil {
			return err
		}
		iMessage, err := x.decode(buf)
		if err != nil {
			return err
		}
		method, seq := iMessage.methodID(), iMessage.seq()
		dec := func(in any) error {
			if err := proto.Unmarshal(iMessage.body(), in.(proto.Message)); err != nil {
				return err
			}
			return nil
		}
		reply, err := x.Methods[method].Handler(x.impl, ctx, dec, x.Unary)
		if err != nil {
			return err
		}
		buf, err := x.encode(seq, method, reply.(proto.Message))
		if err != nil {
			return err
		}
		if err := c.SetWriteDeadline(time.Now().Add(x.connectionTimeout)); err != nil {
			return err
		}
		if _, err := buf.WriteTo(c); err != nil {
			return err
		}
	}
}

func (x *Server) decode(b *bufio.Reader) (message, error) {
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

func (x *Server) encode(seq uint32, method uint16, iMessage proto.Message) (message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*message)
	buf.WriteUint16(uint16(_MESSAGE_HEADER_LENGTH + len(b))) // 2
	buf.WriteUint32(seq)                                     // 4
	buf.WriteUint16(method)                                  // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

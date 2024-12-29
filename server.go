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
	apply(*serverOptions)
}

type funcServerOption struct {
	f func(*serverOptions)
}

func (x *funcServerOption) apply(o *serverOptions) {
	x.f(o)
}

func newFuncServerOption(f func(*serverOptions)) ServerOption {
	return &funcServerOption{
		f: f,
	}
}

func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOptions) {
		o.Unary = i
	})
}

const (
	//defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	//defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

type Handler interface {
	Handle(context.Context, net.Conn)
}

// ListenAndServe binds port and handle requests, blocking until close
func ListenAndServe(ctx context.Context, listener net.Listener, handler Handler) {
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
			//log.Error(err.Error())
			continue
		}
		handler.Handle(ctx, conn)
	}
}

type serverOptions struct {
	// creds             credentials.TransportCredentials
	// unaryInt          grpc.UnaryServerInterceptor
	writeBufferSize   int
	readBufferSize    int
	connectionTimeout time.Duration
	Unary             grpc.UnaryServerInterceptor
}

var defaultServerOptions = serverOptions{
	connectionTimeout: 120 * time.Second,
	writeBufferSize:   defaultWriteBufSize,
	readBufferSize:    defaultReadBufSize,
}

type Server struct {
	serverOptions
	grpc.ServiceRegistrar
	Methods []grpc.MethodDesc
	handler any
}

func NewServer(opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for i := 0; i < len(opt); i++ {
		opt[i].apply(&opts)
	}
	return &Server{serverOptions: opts}
}

func (x *Server) RegisterService(desc *grpc.ServiceDesc, impl any) {
	x.Methods = desc.Methods
	x.handler = impl
}

func (x *Server) Close() error {
	return nil
}

func (x *Server) Serve(ctx context.Context, c net.Conn) (err error) {
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
			if err := proto.Unmarshal(iMessage[6:], in.(proto.Message)); err != nil {
				return err
			}
			return nil
		}
		reply, err := x.Methods[method].Handler(x.handler, ctx, dec, x.Unary)
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

func (x *Server) decode(b *bufio.Reader) (Message, error) {
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

func (x *Server) encode(seq uint16, method uint16, iMessage proto.Message) (Message, error) {
	b, err := proto.Marshal(iMessage)
	if err != nil {
		return nil, err
	}
	buf := pool.Get().(*Message)
	buf.WriteUint16(uint16(6 + len(b))) // 2
	buf.WriteUint16(seq)                // 2
	buf.WriteUint16(method)             // 2
	if _, err := buf.Write(b); err != nil {
		return nil, err
	}
	return *buf, nil
}

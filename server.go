package grpcx

import (
	"bufio"
	"context"
	"errors"
	"net"
	"runtime/debug"
	"time"

	"github.com/vimcoders/go-driver/driver"
	"github.com/vimcoders/go-driver/log"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

const (
	//defaultClientMaxReceiveMessageSize = 1024 * 1024 * 4
	//defaultClientMaxSendMessageSize    = math.MaxInt32
	// http2IOBufSize specifies the buffer size for sending frames.
	defaultWriteBufSize = 32 * 1024
	defaultReadBufSize  = 32 * 1024
)

// ListenAndServe binds port and handle requests, blocking until close
func ListenAndServe(ctx context.Context, listener net.Listener, handler driver.Handler) {
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}
		conn, err := listener.Accept()
		if err != nil {
			log.Error(err.Error())
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

func NewServer(opt ...grpc.ServerOption) *Server {
	return &Server{serverOptions: defaultServerOptions}
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
		iMessage, err := decode(buf)
		if err != nil {
			return err
		}
		method, seq, payload := iMessage.method(), iMessage.seq(), iMessage.payload()
		reply, err := x.Methods[method].Handler(x.handler, ctx, func(in any) error {
			if err := proto.Unmarshal(payload, in.(proto.Message)); err != nil {
				return err
			}
			return nil
		}, nil)
		if err != nil {
			return err
		}
		buf, err := encode(seq, method, reply.(proto.Message))
		if err != nil {
			return err
		}
		if err := c.SetWriteDeadline(time.Now().Add(x.connectionTimeout)); err != nil {
			return err
		}
		if _, err := buf.WriteTo(c); err != nil {
			return err
		}
		buf.reset()
	}
}

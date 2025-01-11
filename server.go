package grpcx

import (
	"bufio"
	"context"
	"errors"
	"net"
	"runtime/debug"
	"time"

	"github.com/vimcoders/grpcx/log"
	"google.golang.org/grpc"
)

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
	defaultReadBufSize = 32 * 1024
)

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
			log.Error(err)
			continue
		}
		log.Info(conn.RemoteAddr())
		go x.serve(ctx, conn)
	}
}

type serverOption struct {
	// creds             credentials.TransportCredentials
	readBufferSize int
	timeout        time.Duration
	Unary          UnaryServerInterceptor
	Methods        []grpc.MethodDesc
}

var defaultServerOptions = serverOption{
	timeout:        120 * time.Second,
	readBufferSize: defaultReadBufSize,
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
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
		if err := x.Close(); err != nil {
			log.Error(err)
		}
	}()
	h := Handler{
		serverOption: x.serverOption,
		Conn:         c,
		impl:         x.impl,
		timeout:      time.Now().Add(x.timeout),
	}
	buf := bufio.NewReaderSize(c, x.readBufferSize)
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
		}
		if err := c.SetReadDeadline(time.Now().Add(x.timeout)); err != nil {
			return err
		}
		req, err := readRequest(buf)
		if err != nil {
			return err
		}
		if err := h.Handle(ctx, req); err != nil {
			return err
		}
	}
}

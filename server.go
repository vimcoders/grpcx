package grpcx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"net"
	"runtime/debug"
	"time"

	"github.com/vimcoders/grpcx/log"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
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

func (x Server) ListenAndServe(ctx context.Context, listener net.Listener, handler Handler) {
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
		go handler.Handle(ctx, conn)
	}
}

func (x Server) Serve(ctx context.Context, listener net.Listener) {
	defer func() {
		if err := recover(); err != nil {
			debug.PrintStack()
		}
	}()
	handler := x.MakeHandler()
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
		go handler.Handle(ctx, conn)
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
	readBufferSize: 32 * 1024,
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

func (x Server) MakeHandler() Handler {
	return &handler{serverOption: x.serverOption, impl: x.impl}
}

type handler struct {
	serverOption
	impl any
}

func (x *handler) do(ctx context.Context, req request) (response, error) {
	seq, cmd := req.seq, req.cmd
	if int(cmd) >= len(x.Methods) {
		var replay []string
		for i := 0; i < len(x.Methods); i++ {
			replay = append(replay, x.Methods[i].MethodName)
		}
		ping, err := json.Marshal(replay)
		if err != nil {
			return response{}, err
		}
		return response{seq: seq, cmd: cmd, b: ping}, nil
	}
	reply, err := x.Methods[req.cmd].Handler(x.impl, ctx, req.dec, x.Unary)
	if err != nil {
		return response{}, err
	}
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return response{}, err
	}
	return response{seq: req.seq, cmd: req.cmd, b: b}, nil
}

func (x *handler) Handle(ctx context.Context, c net.Conn) (err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
		if err := c.Close(); err != nil {
			log.Error(err)
		}
	}()
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
		response, err := x.do(ctx, req)
		if err != nil {
			return err
		}
		if err := c.SetWriteDeadline(time.Now().Add(x.timeout)); err != nil {
			return err
		}
		if _, err := response.WriteTo(c); err != nil {
			return err
		}
	}
}

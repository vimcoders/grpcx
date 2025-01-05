package grpcx

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
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

func (x *Server) do(ctx context.Context, req request) (io.WriterTo, error) {
	seq, cmd := req.seq, req.cmd
	if int(cmd) >= len(x.Methods) {
		response, err := x.NewPingWriter(seq, cmd)
		if err != nil {
			return nil, err
		}
		return response, nil
	}
	dec := func(in any) error {
		if err := proto.Unmarshal(req.b, in.(proto.Message)); err != nil {
			return err
		}
		return nil
	}
	reply, err := x.Methods[cmd].Handler(x.impl, ctx, dec, x.Unary)
	if err != nil {
		return nil, err
	}
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return nil, err
	}
	return &response{
		seq: seq,
		cmd: cmd,
		b:   b,
	}, nil

}

func (x *Server) NewPingWriter(seq, cmd uint16) (io.WriterTo, error) {
	var replay []string
	for i := 0; i < len(x.Methods); i++ {
		replay = append(replay, x.Methods[i].MethodName)
	}
	b, err := json.Marshal(replay)
	if err != nil {
		return nil, err
	}
	return &response{
		seq: seq,
		cmd: cmd,
		b:   b,
	}, nil
}

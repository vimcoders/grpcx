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
	"go.opentelemetry.io/otel/trace"
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

func WithServiceAddr(addr net.Addr) ServerOption {
	return newFuncServerOption(func(o *serverOption) {
		o.Addr = addr
	})
}

func UnaryInterceptor(i UnaryServerInterceptor) ServerOption {
	return newFuncServerOption(func(o *serverOption) {
		o.Unary = i
	})
}

func ListenAndServe(ctx context.Context, impl any, opt ...ServerOption) error {
	svr := NewServer(impl, opt...)
	svr.ListenAndServe(ctx)
	return nil
}

func (x *Server) ListenAndServe(ctx context.Context, opt ...ServerOption) error {
	for i := 0; i < len(opt); i++ {
		opt[i].apply(&x.serverOption)
	}
	ln, err := net.Listen(x.Addr.Network(), x.Addr.String())
	if err != nil {
		return err
	}
	return x.Serve(ctx, ln)
}

func (x *Server) Serve(ctx context.Context, listener net.Listener) error {
	defer func() {
		if err := recover(); err != nil {
			debug.PrintStack()
		}
	}()
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		default:
		}
		conn, err := listener.Accept()
		if err != nil {
			log.Error(err)
			continue
		}
		log.Info(conn.RemoteAddr())
		go x.Handle(ctx, conn)
	}
}

type serverOption struct {
	// creds             credentials.TransportCredentials
	readBufferSize int
	timeout        time.Duration
	Unary          UnaryServerInterceptor
	Methods        []grpc.MethodDesc
	net.Addr
}

var defaultServerOptions = serverOption{
	timeout:        120 * time.Second,
	readBufferSize: 32 * 1024,
}

type Server struct {
	serverOption
	impl any
	Handler
}

func NewServer(impl any, opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for i := 0; i < len(opt); i++ {
		opt[i].apply(&opts)
	}
	return &Server{impl: impl, serverOption: opts}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (s *Server) RegisterService(sd *grpc.ServiceDesc, ss any) {
	s.register(sd, ss)
}

func (s *Server) register(sd *grpc.ServiceDesc, ss any) {
	s.Methods = sd.Methods
	s.impl = ss
}

func (x *Server) Close() error {
	return nil
}

func (x *Server) do(ctx context.Context, req request) (response, error) {
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
		return response{seq: seq, cmd: cmd, payload: ping}, nil
	}
	var span trace.SpanContext
	span.WithTraceID(req.traceID)
	span.WithSpanID(req.spanID)
	method := x.Methods[req.cmd]
	reply, err := method.Handler(x.impl, trace.ContextWithRemoteSpanContext(ctx, span), req.dec, x.Unary)
	if err != nil {
		return response{}, err
	}
	b, err := proto.Marshal(reply.(proto.Message))
	if err != nil {
		return response{}, err
	}
	return response{seq: req.seq, cmd: req.cmd, payload: b}, nil
}

func (x *Server) Handle(ctx context.Context, c net.Conn) (err error) {
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

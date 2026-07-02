package ttrpc

import (
	"context"
	"fmt"
	"log/slog"
	"math"
	"net"
	"path"
	"runtime/debug"
	"time"
	"ttrpc/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type ServerOption interface {
	apply(*ServerOptions)
}

type funcServerOption struct {
	f func(*ServerOptions)
}

func (x *funcServerOption) apply(o *ServerOptions) {
	x.f(o)
}

type ServerOptions struct {
	buffsize             uint16
	timeout              time.Duration
	Methods              map[string]grpc.MethodHandler
	interceptor          grpc.UnaryServerInterceptor
	MaxConcurrentStreams uint32
	codec
}

var defaultServerOptions = ServerOptions{
	timeout:              time.Second * 30,
	buffsize:             math.MaxInt16,
	MaxConcurrentStreams: 16,
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		reply, err := handler(ctx, req)
		if err != nil {
			slog.Error("interceptor",
				"path", info.FullMethod,
				"grpc-req", req,
				"grpc-reply", reply,
				"err:", err,
			)
			return reply, err
		}
		slog.Debug("interceptor",
			"path", info.FullMethod,
			"grpc-req", req,
			"grpc-reply", reply,
		)
		return reply, nil
	},
}

// 自定义拦截器
func UnaryInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return &funcServerOption{
		f: func(so *ServerOptions) {
			so.interceptor = interceptor
		},
	}
}

type Server struct {
	ServerOptions
	impl any
}

func NewServer(opt ...ServerOption) *Server {
	opts := defaultServerOptions
	for i := range opt {
		opt[i].apply(&opts)
	}
	opts.Methods = map[string]grpc.MethodHandler{}
	return &Server{
		ServerOptions: opts,
	}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (x *Server) RegisterService(sd *grpc.ServiceDesc, ss any) {
	x.register(sd, ss)
}

func (x *Server) register(sd *grpc.ServiceDesc, ss any) {
	for i := range sd.Methods {
		x.Methods[path.Join("/", sd.ServiceName, sd.Methods[i].MethodName)] = sd.Methods[i].Handler
	}
	x.impl = ss
}

func (x *Server) Close() error {
	return nil
}

func (x *Server) ListenAndServe(ctx context.Context, addr string, opt ...ServerOption) (err error) {
	for i := range opt {
		opt[i].apply(&x.ServerOptions)
	}
	// 创建TLS监听器
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		panic(err)
	}
	cancelCtx, cancelFunc := context.WithCancel(ctx)
	// 通知所有的协同程序退出
	defer cancelFunc()
	// 接受并处理客户端连接
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		go x.Handle(cancelCtx, conn)
	}
}

func (x *Server) Handle(ctx context.Context, c net.Conn) (err error) {
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			streamID, payload, err := channel.recv()
			if err != nil {
				return err
			}
			var request api.Request
			if err := x.codec.Unmarshal(payload, &request); err != nil {
				return err
			}
			response, err := x.RoundTrip(ctx, &request)
			if err != nil {
				return err
			}
			b, err := x.codec.Marshal(response)
			if err != nil {
				return err
			}
			if err := channel.send(streamID, b); err != nil {
				return err
			}
		}
	}
}

func (x *Server) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	defer func() {
		if e := recover(); e != nil {
			slog.Error(fmt.Sprintf("%s", e))
			debug.PrintStack()
		}
	}()
	handler, ok := x.Methods[req.Metas.Path]
	if !ok {
		return &api.Response{
			Code:    uint32(codes.NotFound),
			Message: fmt.Sprintf("no such method %s", req.Metas.Path),
		}, nil
	}
	reply, err := handler(x.impl, WithAuthority(ctx, req.Metas.Authority), func(in any) error {
		return x.codec.Unmarshal(req.Payload, in)
	}, x.interceptor)
	if err != nil {
		return &api.Response{
			Code:    uint32(codes.Unavailable),
			Message: err.Error(),
		}, nil
	}
	response, err := x.codec.Marshal(reply)
	if err != nil {
		return &api.Response{
			Code:    uint32(codes.Unavailable),
			Message: err.Error(),
		}, nil
	}
	if len(response) > messageLengthMax {
		return &api.Response{
			Code:    uint32(codes.OutOfRange),
			Message: codes.OutOfRange.String(),
		}, nil
	}
	return &api.Response{
		Code:    uint32(codes.OK),
		Message: codes.OK.String(),
		Payload: response,
	}, nil
}

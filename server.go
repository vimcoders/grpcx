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
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/proto"
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
	defer func() {
		if e := recover(); e != nil {
			slog.Error(fmt.Sprintf("%s", e))
			debug.PrintStack()
		}
	}()
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
			h, payload, err := channel.recv()
			if err != nil {
				return err
			}
			var request api.Request
			if err := proto.Unmarshal(payload, &request); err != nil {
				return err
			}
			handler, ok := x.Methods[request.Metas.Path]
			if !ok {
				return NotFound
			}
			reply, err := handler(x.impl, ctx, func(in any) error {
				return x.codec.Unmarshal(payload, in)
			}, x.interceptor)
			response, err := x.codec.Marshal(reply)
			if err != nil {
				return err
			}
			b, err := proto.Marshal(&api.Response{
				Metas:   request.Metas,
				Payload: response,
			})
			if err != nil {
				return err
			}
			if err := channel.writeMessageHeader(messageHeader{
				StreamID: h.StreamID,
				Length:   uint32(len(b)),
			}); err != nil {
				return err
			}
			if len(b) > 0 {
				if _, err := channel.bw.Write(b); err != nil {
					return err
				}
			}
			if err := channel.bw.Flush(); err != nil {
				return err
			}
		}
	}
}

var Unavailable = status.Error(codes.Unavailable, codes.Unavailable.String())                   // 框架默认，可重试
var OK = status.Error(codes.OK, codes.OK.String())                                              // 执行成功
var InvalidArgument = status.Error(codes.InvalidArgument, codes.Internal.String())              // 客户端传参错误
var ResourceExhausted = status.Error(codes.ResourceExhausted, codes.ResourceExhausted.String()) // 告诉客户端退避

var Canceled = status.Error(codes.Canceled, codes.Canceled.String())                      // 被取消
var Unauthenticated = status.Error(codes.Unauthenticated, codes.Unauthenticated.String()) // 需要重新登录 / 换 Token
var Unknown = status.Error(codes.Unknown, codes.Unknown.String())                         // 未知错误
var NotFound = status.Error(codes.NotFound, codes.NotFound.String())                      // 没有找到对应的方法

package transport

import (
	"context"
	"fmt"
	"grpcx/generated/api"
	"grpcx/status"
	"log/slog"
	"net"
	"path"
	"runtime/debug"

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type ServerOption func(c *ServerOptions)

type ServerOptions struct {
	encoding.Codec
	RoundTripper
	impl        any
	sd          *grpc.ServiceDesc
	interceptor grpc.UnaryServerInterceptor
}

var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		return handler(ctx, req)
	},
}

func (x *ServerOptions) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	defer func() {
		if e := recover(); e != nil {
			slog.Error(fmt.Sprintf("%s", e))
			debug.PrintStack()
		}
	}()
	for _, v := range x.sd.Methods {
		path := path.Join("/", x.sd.ServiceName, v.MethodName)
		if path != req.Method {
			continue
		}
		reply, err := v.Handler(x.impl, ctx, func(in any) error {
			return x.Unmarshal(req.Payload, in)
		}, x.interceptor)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		response, err := x.Marshal(reply)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		return &api.Response{
			Code:    int32(codes.OK),
			Message: codes.OK.String(),
			Payload: response,
		}, nil
	}
	return &api.Response{
		Code:    int32(codes.NotFound),
		Message: fmt.Sprintf("no such method %s", req.Method),
	}, nil
}

func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.interceptor = interceptor
	}
}

func WithServiceDesc(sd *grpc.ServiceDesc, impl any) ServerOption {
	return func(so *ServerOptions) {
		so.sd = sd
		so.impl = impl
	}
}

func (x *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			streamID, payload, err := channel.Recv()
			if err != nil {
				return err
			}
			var request api.Request
			if err := x.Unmarshal(payload, &request); err != nil {
				return err
			}
			channel.putmbuf(payload)
			response, err := x.RoundTrip(ctx, &request)
			if err != nil {
				return err
			}
			b, err := x.Marshal(response)
			if err != nil {
				return err
			}
			if err := channel.Send(streamID, b); err != nil {
				return err
			}
		}
	}
}

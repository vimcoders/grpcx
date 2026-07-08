package ttrpc

import (
	"context"
	"grpcx/generated/api"
	"grpcx/status"
	"net"
	"path"

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type ServerOption func(c *ServerOptions)

type ServerOptions struct {
	encoding.Codec
	desc        *grpc.ServiceDesc
	imp         any
	interceptor grpc.UnaryServerInterceptor
	rt          func(context.Context, *api.Request) (*api.Response, error)
}

var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		return handler(ctx, req)
	},
}

func UnaryServerRoundTrip(rt func(context.Context, *api.Request) (*api.Response, error)) ServerOption {
	return func(so *ServerOptions) {
		so.rt = rt
	}
}

func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.interceptor = interceptor
	}
}

func RegisterService(sd *grpc.ServiceDesc, ss any) ServerOption {
	return func(so *ServerOptions) {
		so.desc = sd
		so.imp = ss
	}
}

func (h *ServerOptions) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	if h.rt != nil {
		return h.rt(ctx, req)
	}
	for _, v := range api.EchoService_ServiceDesc.Methods {
		path := path.Join("/", api.EchoService_ServiceDesc.ServiceName, v.MethodName)
		if path != req.Method {
			continue
		}
		reply, err := v.Handler(h.imp, ctx, func(in any) error {
			return h.Unmarshal(req.Payload, in)
		}, h.interceptor)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		response, err := h.Marshal(reply)
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
		Code:    int32(codes.Unimplemented),
		Message: codes.Unimplemented.String(),
	}, nil
}

func (x *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
	defer c.Close()
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

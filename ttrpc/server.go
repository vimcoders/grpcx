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
	RoundTripper
	desc *grpc.ServiceDesc
	imp  any
}

var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
}

func UnaryServerInterceptor(rt RoundTripper) ServerOption {
	return func(so *ServerOptions) {
		so.RoundTripper = rt
	}
}

func RegisterService(sd *grpc.ServiceDesc, ss any) ServerOption {
	return func(so *ServerOptions) {
		so.desc = sd
		so.imp = ss
	}
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

func (h *ServerOptions) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	for _, v := range api.EchoService_ServiceDesc.Methods {
		path := path.Join("/", api.EchoService_ServiceDesc.ServiceName, v.MethodName)
		if path != req.Method {
			continue
		}
		reply, err := v.Handler(h, ctx, func(in any) error {
			return h.Unmarshal(req.Payload, in)
		}, h.Interceptor)
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

func (h *ServerOptions) Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(ctx, req)
}

package ttrpc

import (
	"context"
	"grpcx/generated/api"
	"grpcx/status"
	"net"
	"path"
	"time"

	"grpcx/encoding"

	"grpcx/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type ServerOption func(c *ServerOptions)

type ServerOptions struct {
	encoding.Codec
	desc        *grpc.ServiceDesc
	imp         any
	interceptor grpc.UnaryServerInterceptor
}

var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		return handler(ctx, req)
	},
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

func (so *ServerOptions) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	md := metadata.Pairs(req.Metadatas...)
	for _, v := range so.desc.Methods {
		path := path.Join("/", api.EchoService_ServiceDesc.ServiceName, v.MethodName)
		if path != req.Method {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(req.Timeout))
		defer cancel()
		reply, err := v.Handler(so.imp, metadata.WithMetadata(timeoutCtx, md), func(in any) error {
			return so.Unmarshal(req.Payload, in)
		}, so.interceptor)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		response, err := so.Marshal(reply)
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

func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
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
			if err := so.Unmarshal(payload, &request); err != nil {
				return err
			}
			channel.putmbuf(payload)
			response, err := so.RoundTrip(ctx, &request)
			if err != nil {
				return err
			}
			b, err := so.Marshal(response)
			if err != nil {
				return err
			}
			if err := channel.Send(streamID, b); err != nil {
				return err
			}
		}
	}
}

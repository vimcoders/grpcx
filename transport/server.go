package transport

import (
	"context"
	"net"
	"path"
	"time"

	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx/encoding"

	"github.com/vimcoders/grpcx/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Server is a ttrpc server that handles incoming requests and dispatches them to the appropriate service method.
type ServerOption func(c *ServerOptions)

// ServerOptions is a struct that holds the options for a ttrpc server.
type ServerOptions struct {
	encoding.Codec
	desc        *grpc.ServiceDesc
	imp         any
	interceptor grpc.UnaryServerInterceptor
}

// DefaultServerOptions is the default options for a ttrpc server.
var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		return handler(ctx, req)
	},
}

// NewServer creates a new ttrpc server with the given options.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.interceptor = interceptor
	}
}

// Codec sets the codec for the ttrpc server.
func RegisterService(sd *grpc.ServiceDesc, ss any) ServerOption {
	return func(so *ServerOptions) {
		so.desc = sd
		so.imp = ss
	}
}

func WithServerCodec(codec encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		so.Codec = codec
	}
}

// NewServer creates a new ttrpc server with the given options.
func (so *ServerOptions) RoundTrip(ctx context.Context, channel *channel) error {
	streamID, payload, err := channel.Recv()
	if err != nil {
		return err
	}
	var req api.Request
	s := serverStream{id: streamID, s: channel, Codec: so.Codec, payload: payload}
	if err := encoding.Unmarshal(payload, &req); err != nil {
		code, message := int32(status.InvalidArgument.Code()), err.Error()
		return s.send(&api.Response{Code: code, Message: message})
	}
	defer channel.putmbuf(payload)
	for _, v := range so.desc.Methods {
		if req.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Millisecond)
		defer cancel()
		reply, err := v.Handler(
			so.imp,
			metadata.WithMetadata(timeoutCtx, metadata.Pairs(req.Metadatas...)),
			s.RecvMsg,
			so.interceptor)
		if err != nil {
			code, message := int32(status.Code(err)), err.Error()
			return s.send(&api.Response{Code: code, Message: message})
		}
		return s.SendMsg(reply)
	}
	code, message := int32(codes.Unimplemented), codes.Unimplemented.String()
	return s.send(&api.Response{Code: code, Message: message})
}

// Handle handles incoming requests on the given net.Conn. It reads requests from the connection, dispatches them to the appropriate service method, and writes responses back to the connection.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
	defer c.Close()
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := so.RoundTrip(ctx, channel); err != nil {
				return err
			}
		}
	}
}

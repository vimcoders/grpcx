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
)

// Server is a ttrpc server that handles incoming requests and dispatches them to the appropriate service method.
type ServerOption func(c *ServerOptions)

// ServerOptions is a struct that holds the options for a ttrpc server.
type ServerOptions struct {
	encoding.Codec
	desc        *grpc.ServiceDesc
	imp         any
	interceptor grpc.UnaryServerInterceptor
	Handler
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
	s := serverStream{
		ctx:   ctx,
		id:    streamID,
		s:     channel,
		Codec: so.Codec,
	}
	var req api.Request
	if err := encoding.Unmarshal(payload, &req); err != nil {
		code, message := int32(status.InvalidArgument.Code()), err.Error()
		return s.send(&api.Response{Code: code, Message: message})
	}
	defer channel.putmbuf(payload)
	if err := so.ServeGRPC(&s, &req); err != nil {
		code, message := int32(status.Code(err)), err.Error()
		return s.send(&api.Response{Code: code, Message: message})
	}
	return nil
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

func (so *ServerOptions) ServeGRPC(s Stream, req *api.Request) error {
	for _, v := range so.desc.Methods {
		if req.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(s.Context(), time.Duration(req.Timeout)*time.Millisecond)
		defer cancel()
		dec := func(in any) error {
			return encoding.Unmarshal(req.Payload, in)
		}
		reply, err := v.Handler(
			so.imp,
			metadata.WithMetadata(timeoutCtx, metadata.Pairs(req.Metadatas...)),
			dec, so.interceptor)
		if err != nil {
			return err
		}
		return s.SendMsg(reply)
	}
	return status.Unimplemented.Err()
}

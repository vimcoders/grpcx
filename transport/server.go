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
func (so *ServerOptions) RoundTrip(ctx context.Context, s *serverStream) (err error) {
	for _, v := range so.desc.Methods {
		if s.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(s.Context(), s.Timeout)
		defer cancel()
		reply, err := v.Handler(
			so.imp,
			metadata.WithMetadata(timeoutCtx, s.Metadatas),
			s.RecvMsg, so.interceptor)
		if err != nil {
			return err
		}
		return s.SendMsg(reply)
	}
	for _, v := range so.desc.Streams {
		if s.Method != path.Join("/", so.desc.ServiceName, v.StreamName) {
			continue
		}
		if err := v.Handler(so.imp, s); err != nil {
			return err
		}
	}
	return status.Unimplemented.Err()
}

func (so *ServerOptions) createStream(ctx context.Context, channel *channel) (serverStream, error) {
	streamID, b, err := channel.Recv()
	if err != nil {
		return serverStream{}, err
	}
	defer channel.putmbuf(b)
	var request api.Request
	if err := encoding.Unmarshal(b, &request); err != nil {
		return serverStream{}, err
	}
	return serverStream{
		ctx:       ctx,
		b:         b,
		id:        streamID,
		s:         channel,
		Codec:     so.Codec,
		Method:    request.Method,
		Metadatas: metadata.Pairs(request.Metadatas...),
		Timeout:   time.Duration(request.Timeout) * time.Millisecond,
	}, nil
}

// Handle handles incoming requests on the given net.Conn. It reads requests from the connection, dispatches them to the appropriate service method, and writes responses back to the connection.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) error {
	defer c.Close()
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			s, err := so.createStream(ctx, channel)
			if err != nil {
				return err
			}
			if err := so.RoundTrip(ctx, &s); err != nil {
				code := int32(status.Code(err))
				s.send(&api.Response{Code: code, Message: err.Error()})
			}
		}
	}
}

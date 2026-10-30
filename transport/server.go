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
	desc                    *grpc.ServiceDesc
	imp                     any
	unaryServerInterceptor  grpc.UnaryServerInterceptor
	streamServerInterceptor grpc.StreamServerInterceptor
}

// DefaultServerOptions is the default options for a ttrpc server.
var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	unaryServerInterceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
		return handler(ctx, req)
	},
	streamServerInterceptor: func(srv any, ss grpc.ServerStream, info *grpc.StreamServerInfo, handler grpc.StreamHandler) error {
		return handler(srv, ss)
	},
}

// NewServer creates a new ttrpc server with the given options.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.unaryServerInterceptor = interceptor
	}
}

func StreamServerInterceptor(interceptor grpc.StreamServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.streamServerInterceptor = interceptor
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
func (so *ServerOptions) RoundTrip(ctx context.Context, s grpc.ServerStream, opts ...CallOption) error {
	options := defaultCallOptions
	for _, opt := range opts {
		opt.apply(&options)
	}
	for _, v := range so.desc.Methods {
		if options.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeoutCtx, cancel := context.WithTimeout(s.Context(), options.Timeout)
		defer cancel()
		reply, err := v.Handler(so.imp, timeoutCtx, s.RecvMsg, so.unaryServerInterceptor)
		if err != nil {
			return err
		}
		return s.SendMsg(reply)
	}
	for _, v := range so.desc.Streams {
		if options.Method != path.Join("/", so.desc.ServiceName, v.StreamName) {
			continue
		}
		info := &grpc.StreamServerInfo{
			FullMethod:     v.StreamName,
			IsClientStream: v.ClientStreams,
			IsServerStream: v.ServerStreams,
		}
		if err := so.streamServerInterceptor(so.imp, s, info, v.Handler); err != nil {
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
	metadatas := metadata.Pairs(request.Metadatas...)
	return serverStream{
		b:         b,
		s:         channel,
		id:        streamID,
		Codec:     so.Codec,
		Metadatas: metadatas,
		Method:    request.Method,
		ctx:       metadata.WithMetadata(ctx, metadatas),
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
			if err := so.RoundTrip(ctx, &s, &s.CallOptions); err != nil {
				code := int32(status.Code(err))
				s.send(&api.Response{Code: code, Message: err.Error()})
			}
		}
	}
}

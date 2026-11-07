package transport

import (
	"cmp"
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
	Options
	desc                    *grpc.ServiceDesc
	imp                     any
	unaryServerInterceptor  grpc.UnaryServerInterceptor
	streamServerInterceptor grpc.StreamServerInterceptor
}

// DefaultServerOptions is the default options for a ttrpc server.
var DefaultServerOptions = ServerOptions{
	Options: DefaultOptions(),
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

// Handle handles incoming requests on the given net.Conn. It reads requests from the connection, dispatches them to the appropriate service method, and writes responses back to the connection.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) error {
	channel := newChannel(c)
	defer channel.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := so.roundtrip(ctx, channel); err != nil {
				return err
			}
		}
	}
}

func (so *ServerOptions) roundtrip(ctx context.Context, channel *channel) error {
	streamID, h, p, err := readCall(channel)
	if err != nil {
		return err
	}
	defer putmbuf(p)
	metadatas := metadata.Pairs(h.Metadatas...)
	s := &serverStream{
		p:         p,
		Sender:    channel,
		id:        streamID,
		Codec:     so.Codec,
		Metadatas: metadatas,
		Method:    h.Method,
		ctx:       metadata.WithMetadata(ctx, metadatas),
		Timeout:   time.Duration(h.Timeout) * time.Millisecond,
	}
	if so.desc == nil {
		return s.send(&api.Response{
			Header: &api.Header{
				Code:    int32(codes.Unimplemented),
				Message: "grpc.ServiceDesc zero",
			},
		})
	}
	for _, v := range so.desc.Methods {
		if s.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeout := cmp.Or(s.Timeout, so.Timeout())
		timeoutCtx, cancel := context.WithTimeout(s.Context(), timeout)
		defer cancel()
		reply, err := v.Handler(so.imp, timeoutCtx, s.RecvMsg, so.unaryServerInterceptor)
		if err != nil {
			return s.send(&api.Response{
				Header: &api.Header{
					Code:    int32(status.Code(err)),
					Message: err.Error(),
				},
			})
		}
		return s.SendMsg(reply)
	}
	for _, v := range so.desc.Streams {
		if s.Method != path.Join("/", so.desc.ServiceName, v.StreamName) {
			continue
		}
		info := &grpc.StreamServerInfo{
			FullMethod:     v.StreamName,
			IsClientStream: v.ClientStreams,
			IsServerStream: v.ServerStreams,
		}
		return so.streamServerInterceptor(so.imp, s, info, v.Handler)
	}
	return s.send(&api.Response{
		Header: &api.Header{
			Code:    int32(codes.Unimplemented),
			Message: "Unimplemented",
		},
	})
}

package transport

import (
	"cmp"
	"context"
	"net"
	"path"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

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
	desc: &grpc.ServiceDesc{},
}

// NewServer creates a new ttrpc server with the given options.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.unaryServerInterceptor = interceptor
	}
}

// Codec sets the codec for the ttrpc server.
func RegisterService(sd *grpc.ServiceDesc, ss any) ServerOption {
	return func(so *ServerOptions) {
		if sd != nil {
			so.desc = sd
		}
		if ss != nil {
			so.imp = ss
		}
	}
}

func WithServerCodec(codec encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		so.codec = codec
	}
}

func WithServerReadTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.readTimeout = d
	}
}

func WithServerWriteTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.writeTimeout = d
	}
}

func WithServerBufSize(n int) ServerOption {
	return func(so *ServerOptions) {
		so.bufsize = min(n, defaultMaxBufSize/defaultBufSize)
	}
}

func StreamServerInterceptor(interceptor grpc.StreamServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.streamServerInterceptor = interceptor
	}
}

// Handle handles incoming requests on the given net.Conn. It reads requests from the connection, dispatches them to the appropriate service method, and writes responses back to the connection.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) error {
	channel := newChannel(c, WithBufSize(so.bufsize))
	defer channel.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := so.interceptor(ctx, channel); err != nil {
				return err
			}
		}
	}
}

func (so *ServerOptions) interceptor(ctx context.Context, channel *channel) error {
	streamID, h, err := channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(h)
	var header api.Header
	if err := encoding.Unmarshal(h, &header); err != nil {
		return err
	}
	metadatas := metadata.Pairs(header.Metadatas...)
	s := &serverStream{
		channel: channel,
		id:      streamID,
		codec:   so.codec,
		ctx:     metadata.WithMetadata(ctx, metadatas),
	}
	for _, v := range so.desc.Methods {
		if header.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeout := cmp.Or(time.Duration(header.Timeout)*time.Millisecond, so.timeout, defaultTimeout)
		timeoutCtx, cancel := context.WithTimeout(s.Context(), timeout)
		defer cancel()
		reply, err := v.Handler(so.imp, timeoutCtx, s.RecvMsg, so.unaryServerInterceptor)
		if err != nil {
			return s.senderr(status.Code(err), err.Error())
		}
		return s.SendMsg(reply)
	}
	for _, v := range so.desc.Streams {
		if header.Method != path.Join("/", so.desc.ServiceName, v.StreamName) {
			continue
		}
		info := &grpc.StreamServerInfo{
			FullMethod:     header.Method,
			IsClientStream: v.ClientStreams,
			IsServerStream: v.ServerStreams,
		}
		return so.streamServerInterceptor(so.imp, s, info, v.Handler)
	}
	_, p, err := channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(p)
	return s.senderr(codes.Unimplemented, header.Method)
}

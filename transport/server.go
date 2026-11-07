package transport

import (
	"cmp"
	"context"
	"net"
	"path"
	"time"

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
	desc                   *grpc.ServiceDesc
	imp                    any
	unaryServerInterceptor grpc.UnaryServerInterceptor
}

// DefaultServerOptions is the default options for a ttrpc server.
var DefaultServerOptions = ServerOptions{
	Options: DefaultOptions(),
	unaryServerInterceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
		return handler(ctx, req)
	},
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
		so.desc = sd
		so.imp = ss
	}
}

func WithServerCodec(codec encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		so.Codec = codec
	}
}

func WithServerReadTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.readTimeout = d
	}
}

func WithServerWriteTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.readTimeout = d
	}
}

func WithServerBufSize(n int) ServerOption {
	return func(so *ServerOptions) {
		so.bufsize = min(n*defaultBufSize, defaultMaxBufSize)
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
			if err := so.interceptor(ctx, channel); err != nil {
				return err
			}
		}
	}
}

func (so *ServerOptions) interceptor(ctx context.Context, channel *channel) error {
	streamID, h, p, err := readCall(channel)
	if err != nil {
		return err
	}
	defer putmbuf(p)
	metadatas := metadata.Pairs(h.Metadatas...)
	s := &serverStream{
		p:      p,
		header: h,
		Sender: channel,
		id:     streamID,
		codec:  so.Codec,
		ctx:    metadata.WithMetadata(ctx, metadatas),
	}
	if so.desc == nil {
		return s.senderr(codes.Unimplemented, "grpc.ServiceDesc zero")
	}
	for _, v := range so.desc.Methods {
		if h.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeout := cmp.Or(time.Duration(h.Timeout)*time.Millisecond, so.timeout, defaultTimeout)
		timeoutCtx, cancel := context.WithTimeout(s.Context(), timeout)
		defer cancel()
		reply, err := v.Handler(so.imp, timeoutCtx, s.RecvMsg, so.unaryServerInterceptor)
		if err != nil {
			return s.senderr(codes.Internal, err.Error())
		}
		return s.SendMsg(reply)
	}
	for _, v := range so.desc.Streams {
		if h.Method != path.Join("/", so.desc.ServiceName, v.StreamName) {
			continue
		}
		return v.Handler(so.imp, s)
	}
	return s.senderr(codes.Unimplemented, codes.NotFound.String())
}

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

// ServerOption configures a ServerOptions.
type ServerOption func(c *ServerOptions)

// ServerOptions holds the options for a grpcx server: the registered service
// descriptor and implementation, the unary interceptor, plus the embedded
// transport Options shared with the client side.
type ServerOptions struct {
	Options
	desc                   *grpc.ServiceDesc
	imp                    any
	unaryServerInterceptor grpc.UnaryServerInterceptor
}

// DefaultServerOptions is the default configuration for a grpcx server: the
// standard transport options, a pass-through unary interceptor and an empty
// service descriptor.
var DefaultServerOptions = ServerOptions{
	Options: DefaultOptions(),
	unaryServerInterceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
		return handler(ctx, req)
	},
	desc: &grpc.ServiceDesc{},
}

// UnaryServerInterceptor sets the interceptor that wraps every unary call,
// receiving the server implementation, the request, the call info and the
// handler to invoke.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.unaryServerInterceptor = interceptor
	}
}

// RegisterService registers the service descriptor and its implementation.
// nil arguments are ignored so an already registered service is kept.
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

// WithServerCodec sets the codec used to unmarshal requests and marshal
// replies on the server side.
func WithServerCodec(codec encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		so.codec = codec
	}
}

// WithServerReadTimeout sets how long the server waits for the next frame
// before giving up on the connection.
func WithServerReadTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.readTimeout = d
	}
}

// WithServerWriteTimeout sets how long the server waits for a write to flush
// before failing the call.
func WithServerWriteTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.writeTimeout = d
	}
}

// WithServerBufSize caps the send buffer in blocks of defaultBufSize,
// clamped to defaultMaxBufSize/defaultBufSize. A value of zero disables
// batching and makes every Send synchronous.
func WithServerBufSize(n int) ServerOption {
	return func(so *ServerOptions) {
		so.bufsize = min(n, defaultMaxBufSize/defaultBufSize)
	}
}

// Handle serves the connection until it breaks: it creates the channel, then
// loops reading one frame at a time and dispatching it through interceptor.
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

// interceptor reads one request frame, resolves the target method from the
// header, and runs its unary handler. An unknown method is answered with
// codes.Unimplemented.
func (so *ServerOptions) interceptor(ctx context.Context, channel *channel) error {
	streamID, h, err := channel.Recv()
	if err != nil {
		return err
	}
	defer putmbuf(h)
	var opts api.Header
	if err := encoding.Unmarshal(h, &opts); err != nil {
		return err
	}
	metadatas := metadata.Pairs(opts.Metadatas...)
	s := &unaryStream{
		channel: channel,
		id:      streamID,
		codec:   so.codec,
		ctx:     metadata.WithMetadata(ctx, metadatas),
	}
	defer s.Close()
	for _, v := range so.desc.Methods {
		if opts.Method != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		timeout := cmp.Or(time.Duration(opts.Timeout)*time.Millisecond, so.timeout, defaultTimeout)
		timeoutCtx, cancel := context.WithTimeout(s.Context(), timeout)
		defer cancel()
		reply, err := v.Handler(so.imp, timeoutCtx, s.Recv, so.unaryServerInterceptor)
		if err != nil {
			code, desc := uint32(status.Code(err)), err.Error()
			return s.Send(&api.Empty{}, &CallOptions{Code: code, Desc: desc})
		}
		return s.Send(reply, &CallOptions{Code: uint32(codes.OK)})
	}
	code, desc := uint32(codes.Unimplemented), opts.Method
	return s.Send(&api.Empty{}, &CallOptions{Code: code, Desc: desc})
}

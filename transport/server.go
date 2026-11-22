package transport

import (
	"context"
	"net"
	"path"
	"time"

	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/encoding"

	"google.golang.org/grpc"
)

// ServerOption configures a ServerOptions.
type ServerOption func(c *ServerOptions)

// ServerOptions holds the options for a grpcx server: the registered service
// descriptor and implementation, the unary interceptor, the handler that serves
// each connection, plus the embedded transport Options shared with the client
// side. The built-in handler serves unary methods only, so there is no stream
// interceptor to configure.
type ServerOptions struct {
	Options
	desc     *grpc.ServiceDesc
	imp      any
	unaryInt grpc.UnaryServerInterceptor
	Handler
}

// DefaultServerOptions is the default configuration for a grpcx server: the
// standard transport options, a pass-through unary interceptor and an empty
// service descriptor.
var DefaultServerOptions = ServerOptions{
	Options: DefaultOptions(),
	unaryInt: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
		return handler(ctx, req)
	},
	desc: &grpc.ServiceDesc{},
}

// UnaryServerInterceptor sets the interceptor that wraps every unary call,
// receiving the server implementation, the request, the call info and the
// handler to invoke.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.unaryInt = interceptor
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

// WithServerHandler replaces the built-in per-connection handler with h. The
// built-in one resolves a request to a registered unary method and answers
// anything else with codes.Unimplemented; a replacement owns every frame read
// from and written to the connection, which is how a proxy or an alternative
// protocol runs on the same listener. A nil Handler keeps the built-in one.
func WithServerHandler(h Handler) ServerOption {
	return func(so *ServerOptions) {
		if h != nil {
			so.Handler = h
		}
	}
}

// Handle serves the connection until it breaks: when a Handler is installed it
// runs that one, otherwise it wraps the connection in a channel and loops
// dispatching one call at a time through interceptor.
func (s *ServerOptions) Handle(ctx context.Context, c net.Conn) error {
	if s.Handler != nil {
		return s.Handler.Handle(ctx, c)
	}
	ch := newChannel(c, WithBufSize(s.bufsize))
	defer ch.Close()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := s.interceptor(ctx, ch, ch); err != nil {
				return err
			}
		}
	}
}

// interceptor reads one request frame, resolves the target method from the
// header, and dispatches it to the matching unary method handler, passing recv
// for the request payload and send for the reply. Anything that does not
// resolve to a registered unary method — an unknown name, or a stream method,
// which grpcx does not serve — is answered with codes.Unimplemented.
func (s *ServerOptions) interceptor(ctx context.Context, recv Receiver, send Sender) error {
	options, err := decodeCallOptions(recv)
	if err != nil {
		return err
	}
	for _, v := range s.desc.Methods {
		if options.FullMethodName != path.Join("/", s.desc.ServiceName, v.MethodName) {
			continue
		}
		s := &defaultMethodHandler{
			Sender:   send,
			Options:  s.Options,
			id:       options.StreamID,
			imp:      s.imp,
			unaryInt: s.unaryInt,
			Handler:  v.Handler,
		}
		return s.Handle(ctx, recv, WithCallOptions(options))
	}
	unimplemented := empty{id: options.StreamID, codec: s.codec, Sender: send}
	return unimplemented.Handle(ctx, recv, WithCallOptions(options))
}

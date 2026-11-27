package transport

import (
	"cmp"
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
// descriptor and implementation, the unary interceptor, plus the transport
// Options shared with the client side.
//
// It also implements Handler, which is what serves each accepted connection.
// The built-in handler answers unary methods only, so there is no stream
// interceptor to configure.
type ServerOptions struct {
	Options
	desc     *grpc.ServiceDesc
	imp      any
	unaryInt grpc.UnaryServerInterceptor
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

// WithServerCodec sets the codec for request and reply bodies on the server
// side. The call header is always decoded with the built-in codec in package
// encoding, so a client and a server agree on it whatever each side configured
// here.
func WithServerCodec(codec encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		so.codec = codec
	}
}

// WithServerReadTimeout sets how long the server waits for the next frame
// before giving up on the connection. Handle passes it to the frame channel of
// every accepted connection, where it arms the header read; the payload read
// shares that deadline. Zero falls back to defaultReadTimeout.
func WithServerReadTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.readTimeout = cmp.Or(d, defaultReadTimeout)
	}
}

// WithServerWriteTimeout sets how long the server waits for a write to flush
// before failing the call. Handle passes it to the frame channel of every
// accepted connection; both the synchronous write and the batched flush arm
// the deadline themselves. Zero falls back to defaultWriteTimeout.
func WithServerWriteTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.writeTimeout = cmp.Or(d, defaultWriteTimeout)
	}
}

// WithServerBufSize caps the send buffer in blocks of defaultBufSize, clamped
// to defaultMaxBufSize/defaultBufSize. Zero disables batching and makes every
// Send synchronous.
func WithServerBufSize(n int) ServerOption {
	return func(so *ServerOptions) {
		so.bufsize = min(n, defaultMaxBufSize/defaultBufSize)
	}
}

// WithServerTimeout sets the fallback deadline for a call whose header names no
// timeout. The option only stores the value; Handle applies it under the
// defaultTimeout ceiling, so a value above that never lets a handler run longer,
// and zero falls through to the ceiling at use time.
func WithServerTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		so.timeout = d
	}
}

// Handle serves the connection until it breaks: it wraps the connection in a
// channel and dispatches one call at a time through interceptor.
//
// One call at a time is by design: the wire is a single length-prefixed frame
// stream per connection, so reads cannot run in parallel without risking a
// frame boundary. Concurrency comes from connections instead — the K8s Service
// in front of the pods fans requests out across them.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) error {
	ch := newChannel(c, WithBufSize(so.bufsize), WithReadTimeout(so.readTimeout), WithWriteTimeout(so.writeTimeout))
	defer func() { _ = ch.Close() }()
	stop := context.AfterFunc(ctx, func() { _ = ch.Close() })
	defer stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			if err := so.interceptor(ctx, ch); err != nil {
				return err
			}
		}
	}
}

// interceptor reads one request frame, resolves the target method from the
// header, and dispatches it to the matching unary method handler, handing it the
// same channel for the request payload and the reply.
//
// Anything that does not resolve to a registered unary method — an unknown
// name, or a stream method, which grpcx does not serve — is answered with
// codes.Unimplemented. The lookup walks the method table and compares with
// path.Join each entry, so it is linear per request.
func (so *ServerOptions) interceptor(ctx context.Context, ch Channel) error {
	options, err := decodeCallOptions(ch)
	if err != nil {
		return err
	}
	for _, v := range so.desc.Methods {
		if options.FullMethodName != path.Join("/", so.desc.ServiceName, v.MethodName) {
			continue
		}
		s := &defaultMethodHandler{
			Sender:   ch,
			Options:  so.Options,
			imp:      so.imp,
			unaryInt: so.unaryInt,
			Handler:  v.Handler,
		}
		return s.Handle(ctx, ch, WithCallOptions(options))
	}
	e := empty{Codec: so.codec, Sender: ch}
	return e.Handle(ctx, ch, WithCallOptions(options))
}

// RegisterService records the service description and implementation the
// connection serves. A server holds one service: a later registration replaces
// the earlier one, because the dispatch table has a single entry.
func (so *ServerOptions) RegisterService(sd *grpc.ServiceDesc, ss any) {
	if sd != nil {
		so.desc = sd
	}
	if ss != nil {
		so.imp = ss
	}
}

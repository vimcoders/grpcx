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
// descriptor and implementation, the unary interceptor, the transport Options
// shared with the client side, and the Codec that encodes replies and decodes
// requests.
//
// Codec is derived state, not an independent setting: WithServerBufSize rebuilds
// it from Options.ecodec on every call, picking codec when batching is on and
// defaultCodec when it is off. WithServerCodec leans on that by writing ecodec
// and then calling WithServerBufSize again.
//
// ServerOptions also implements Handler, which is what serves each accepted
// connection. The built-in handler answers unary methods only, so there is no
// stream interceptor to configure.
type ServerOptions struct {
	Options
	desc     *grpc.ServiceDesc
	imp      any
	unaryInt grpc.UnaryServerInterceptor
	Codec
}

// DefaultServerOptions returns a fresh ServerOptions for every call: the
// standard transport options, a pass-through unary interceptor, an empty
// service descriptor and a defaultCodec built from the default ecodec. It is a
// function rather than a package variable so each server owns its own Codec.
func DefaultServerOptions() ServerOptions {
	opt := DefaultOptions()
	return ServerOptions{
		Options: opt,
		unaryInt: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (reply any, err error) {
			return handler(ctx, req)
		},
		desc:  &grpc.ServiceDesc{},
		Codec: &defaultCodec{opt.ecodec},
	}
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

// WithServerCodec stores c in Options.ecodec and rebuilds the server Codec by
// re-running WithServerBufSize with the current threshold, so the choice
// between defaultCodec and codec is re-derived from the new ecodec. Option
// order does not matter: whichever of the two options runs last leaves a Codec
// built from the latest ecodec.
//
// The request header is decoded with the built-in codec of package encoding
// whatever this option holds. The reply header is marshalled by the server
// Codec, so under the default codec it goes out as protobuf while a custom
// ecodec encodes it as that codec would.
func WithServerCodec(c encoding.Codec) ServerOption {
	return func(so *ServerOptions) {
		WithCodec(c)(&so.Options)
		WithServerBufSize(so.bufsize)(so)
	}
}

// WithServerReadTimeout sets how long the server waits for the next frame
// before giving up on the connection. Handle passes it to the frame channel of
// every accepted connection, where it arms the header read; the payload read
// shares that deadline. Zero falls back to defaultReadTimeout.
func WithServerReadTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		WithReadTimeout(d)(&so.Options)
	}
}

// WithServerWriteTimeout sets how long the server waits for a write to flush
// before failing the call. Handle passes it to the frame channel of every
// accepted connection; both the synchronous write and the batched flush arm
// the deadline themselves. Zero falls back to defaultWriteTimeout.
func WithServerWriteTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		WithWriteTimeout(d)(&so.Options)
	}
}

// WithServerBufSize applies WithBufSize and then rebuilds the server Codec from
// Options.ecodec: codec when the threshold is positive, defaultCodec otherwise.
// Rebuilding on every call is what keeps the Codec in step with whichever of
// WithServerBufSize and WithServerCodec ran last.
//
// With batching on, codec keeps every reply — including the reply header and a
// proto.Message body — out of the frame pool, so sender.send can free its
// buffers when it returns without recycling bytes that are still queued for the
// flush. With batching off, defaultCodec puts proto messages in pool buffers.
//
// WithBufSize stores n as given, so a non-positive n writes a non-positive
// threshold and turns batching off; the Codec rebuilt here is then
// defaultCodec, which puts the pool back in use for proto messages.
func WithServerBufSize(n int) ServerOption {
	return func(so *ServerOptions) {
		if n < 0 {
			return
		}
		WithBufSize(n)(&so.Options)
		if so.bufsize == 0 {
			so.Codec = &defaultCodec{so.ecodec}
		} else {
			so.Codec = &codec{so.ecodec}
		}
	}
}

// WithServerTimeout sets the fallback deadline for a call whose header names no
// timeout. The option only stores the value; Handle applies it under the
// defaultTimeout ceiling, so a value above that never lets a handler run longer,
// and zero falls through to the ceiling at use time.
func WithServerTimeout(d time.Duration) ServerOption {
	return func(so *ServerOptions) {
		WithTimeout(d)(&so.Options)
	}
}

// Handle serves the connection until it breaks: it wraps the connection in a
// channel and dispatches one call at a time through interceptor. The channel is
// built from bufsize, readTimeout and writeTimeout only; the server Codec is
// passed to the handlers by interceptor, not to the channel.
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

// interceptor reads one request header, resolves the target method from it, and
// dispatches to the matching unary handler, handing it the same channel for the
// request payload and the reply. The handler gets the server Codec and only the
// timeout out of Options: defaultMethodHandler no longer embeds Options, so the
// other settings stay on the channel.
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
			Codec:    so.Codec,
			timeout:  so.timeout,
			imp:      so.imp,
			unaryInt: so.unaryInt,
			Handler:  v.Handler,
		}
		return s.Handle(ctx, ch, WithCallOptions(options))
	}
	e := empty{Codec: so.Codec, Sender: ch}
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

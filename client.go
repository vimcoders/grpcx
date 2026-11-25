package grpcx

import (
	"cmp"
	"context"
	"io"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

// ClientConnInterface is the client-side connection: unary and framed calls
// plus Close.
type ClientConnInterface interface {
	grpc.ClientConnInterface
	RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error)
	io.Closer
}

// Option configures a client before it dials.
type Option func(c *client)

// WithCodec sets the codec used to marshal requests and unmarshal responses.
func WithCodec(codec encoding.Codec) Option {
	return func(c *client) {
		c.Codec = codec
	}
}

// WithUnaryClientInterceptor wraps every unary call with i. The default
// interceptor picks a backend from the balancer and forwards the call to it.
func WithUnaryClientInterceptor(i UnaryClientInterceptor) Option {
	return func(c *client) {
		c.unaryClientInterceptor = i
	}
}

// WithTimeout sets the default call timeout. The value is forwarded to the
// transport, which writes it into the request header. A caller deadline replaces
// it, shorter or longer: the default only bounds a call that names no deadline.
func WithTimeout(d time.Duration) Option {
	return func(c *client) {
		c.opts = append(c.opts, transport.WithTimeout(d))
	}
}

// WithMaxStreams caps how many concurrent calls one connection may carry. Zero
// falls back to the transport default.
func WithMaxStreams(n uint32) Option {
	return func(c *client) {
		c.opts = append(c.opts, transport.WithMaxStreams(n))
	}
}

// WithBufSize sets the send buffer in blocks of the transport block size. Zero
// disables batching and makes every send synchronous.
func WithBufSize(n int) Option {
	return func(c *client) {
		c.opts = append(c.opts, transport.WithBufSize(n))
	}
}

// client is the ClientConnInterface implementation: it holds the codec, the
// unary interceptor, the transport options and the balancer picker. The
// picker's Close is what stops the keepalive loop, so the client keeps no cancel
// function of its own.
type client struct {
	balancer.Picker
	encoding.Codec
	unaryClientInterceptor UnaryClientInterceptor
	opts                   []transport.Option
}

// DialContext builds a client for endpoint: it installs the default unary
// interceptor, applies the options, dials through the balancer under the caller's
// ctx and keeps the resulting picker.
func DialContext(ctx context.Context, endpoint string, opts ...Option) (ClientConnInterface, error) {
	c := client{
		Codec: encoding.GetCodec(encoding.Name),
		unaryClientInterceptor: func(ctx context.Context, method string, req, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			return rt.Invoke(ctx, method, req, reply, opts...)
		},
	}
	for _, o := range opts {
		o(&c)
	}
	c.opts = append(c.opts, transport.WithCodec(c.Codec))
	picker, err := balancer.DialContext(ctx, endpoint, c.opts...)
	if err != nil {
		return nil, err
	}
	c.Picker = picker
	return &c, nil
}

// Dial connects to endpoint using the background context.
func Dial(endpoint string, opts ...Option) (ClientConnInterface, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

// Invoke performs one unary call through the unary client interceptor.
func (c *client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	if err := c.unaryClientInterceptor(ctx, method, req, reply, c.Picker, opts...); err != nil {
		return err
	}
	return nil
}

// RoundTrip picks a backend by the full method name in req.Options and sends
// the framed request through it.
func (c *client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	options := cmp.Or(req.Options, &api.CallOptions{})
	info := balancer.PickInfo{
		FullMethodName: options.FullMethodName,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(ctx, req)
}

// NewStream rejects every streaming call with codes.Unimplemented: grpcx
// carries unary calls only, and there is no stream client interceptor to hand
// the call to. desc, method and opts are all ignored.
func (c *client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

// Close closes the picker, which cancels the balancer's context. The keepalive
// loop returns on that cancel and empties the pool, but it does not close the
// backend transports: each one is closed by its own receive loop once the read
// deadline (defaultReadTimeout, 30s) fires. Close therefore returns without
// blocking, and the fds are released a read timeout later.
func (c *client) Close() error {
	return c.Picker.Close()
}

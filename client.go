package grpcx

import (
	"context"
	"io"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/mem"
)

// CallOption is transport.CallOption, re-exported so a caller that only relays
// bytes can configure a call without importing the transport package. The
// interface has a single concrete implementation, CallOptions, re-exported just
// below.
type CallOption = transport.CallOption

// CallOptions is transport.CallOptions, the value a caller builds to carry the
// method name, the metadata pairs and the timeout of a framed call. Re-exported
// for the same reason: naming the type should not cost an import.
type CallOptions = transport.CallOptions

// ClientConnInterface is the client-side connection: unary and framed calls
// plus Close.
type ClientConnInterface interface {
	grpc.ClientConnInterface
	RoundTrip(context.Context, []byte, ...CallOption) (mem.Buffer, error)
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

// WithTimeout stores the per-call timeout on the transport options. No request
// path reads it: a call is bounded by the caller's context deadline, and every
// stream additionally by the transport's read timeout. Ping is the one reader,
// capping it at the keepalive period so a health check fails before the next one
// is due. Zero leaves the default in place.
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

// RoundTrip picks a backend and sends the framed payload through it. The pick
// carries no method name: backends come from the K8s Service or DNS, so every
// endpoint is equally eligible and the method only travels as frame content. The
// reply comes back as the bytes the backend wrote: how to interpret them is the
// caller's business, not this client's.
func (c *client) RoundTrip(ctx context.Context, p []byte, opts ...CallOption) (mem.Buffer, error) {
	rt, err := c.Pick(ctx, balancer.PickInfo{})
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(ctx, p, opts...)
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
// deadline (defaultReadTimeout, three keepalive periods) fires. Close therefore
// returns without blocking, and the fds are released a read timeout later.
func (c *client) Close() error {
	return c.Picker.Close()
}

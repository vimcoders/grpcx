package grpcx

import (
	"context"
	"io"
	"time"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
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

// WithStreamClientInterceptor wraps every stream call with i. grpcx carries
// unary calls only, so this exists for interface compatibility.
func WithStreamClientInterceptor(i StreamClientInterceptor) Option {
	return func(c *client) {
		c.streamClientInterceptor = i
	}
}

// WithTimeout sets the default call timeout. The value is forwarded to the
// transport, which writes it into the request header, and is overridden by a
// shorter caller deadline.
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
// interceptors, the transport options and the balancer picker.
type client struct {
	balancer.Picker
	encoding.Codec
	unaryClientInterceptor  UnaryClientInterceptor
	streamClientInterceptor StreamClientInterceptor
	opts                    []transport.Option
}

// DialContext builds a client for endpoint: it installs the default
// interceptors, applies the options, dials through the balancer and keeps the
// resulting picker.
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
		streamClientInterceptor: func(ctx context.Context, desc *grpc.StreamDesc, balance balancer.Picker, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return nil, err
			}
			return rt.NewStream(ctx, desc, method, opts...)
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

// RoundTrip picks a backend by the method in req.Header and sends the framed
// request through it.
func (c *client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	info := balancer.PickInfo{
		FullMethodName: req.Header.Method,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(ctx, req)
}

// NewStream opens a stream through the stream client interceptor. grpcx carries
// unary calls only, so the returned stream cannot actually carry frames.
func (c *client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return c.streamClientInterceptor(ctx, desc, c.Picker, method, opts...)
}

// Close closes the balancer, which closes every backend transport.
func (c *client) Close() error {
	return c.Picker.Close()
}

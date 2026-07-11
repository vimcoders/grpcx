package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/generated/api"
	"grpcx/ttrpc"
	"time"

	"grpcx/encoding"

	"google.golang.org/grpc"
)

// Client is a ttrpc client that handles outgoing requests and dispatches them to the appropriate transport.
type Option func(c *Client)

// WithCodec sets the codec for the ttrpc client.
func WithCodec(c encoding.Codec) Option {
	return func(c *Client) {
		c.Codec = c
	}
}

// WithBalancer sets the balancer for the ttrpc client.
func WithBalancer(b balancer.Picker) Option {
	return func(c *Client) {
		c.Picker = b
	}
}

// WithUnaryClientInterceptor sets the unary client interceptor for the ttrpc client.
func WithUnaryClientInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Client) {
		c.interceptor = i
	}
}

// WithTimeout sets the timeout for the ttrpc client.
func WithTimeout(d time.Duration) Option {
	return func(c *Client) {
		c.opts = append(c.opts, ttrpc.WithTimeout(d))
	}
}

// WithMaxStreams sets the maximum number of streams for the ttrpc client.
func WithMaxStreams(n int) Option {
	return func(c *Client) {
		c.opts = append(c.opts, ttrpc.WithMaxStreams(n))
	}
}

// Client is a ttrpc client that handles outgoing requests and dispatches them to the appropriate transport.
type Client struct {
	balancer.Picker
	encoding.Codec
	interceptor UnaryClientInterceptor
	grpc.UnaryClientInterceptor
	opts []ttrpc.Option
}

func DialContext(ctx context.Context, endpoint string, opts ...Option) (ttrpc.RoundTripper, error) {
	c := Client{
		Codec: encoding.GetCodec(encoding.Name),
		interceptor: func(ctx context.Context, method string, req, reply any, rt ttrpc.RoundTripper, opts ...grpc.CallOption) error {
			return rt.Invoke(ctx, method, req, reply, opts...)
		},
	}
	for _, o := range opts {
		o(&c)
	}
	picker, err := balancer.DialContext(ctx, endpoint, c.opts...)
	if err != nil {
		return nil, err
	}
	c.Picker = picker
	return &c, nil
}

func Dial(endpoint string, opts ...Option) (ttrpc.RoundTripper, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	info := balancer.PickInfo{
		FullMethodName: method,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return err
	}
	if err := c.interceptor(ctx, method, req, reply, rt, opts...); err != nil {
		return err
	}
	return nil
}

func (c *Client) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	info := balancer.PickInfo{
		FullMethodName: req.Method,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return nil, err
	}
	return rt.RoundTrip(ctx, req)
}

// NewStream creates a new stream with the given stream descriptor to the
// specified service and method. If not a streaming client, the request object
// may be provided.
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	info := balancer.PickInfo{
		FullMethodName: method,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return nil, err
	}
	return rt.NewStream(ctx, desc, method, opts...)
}

func (c *Client) Close() error {
	return c.Picker.Close()
}

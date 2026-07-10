package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/generated/api"
	"grpcx/ttrpc"

	"grpcx/encoding"

	"google.golang.org/grpc"
)

type Option func(c *Client)

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

func WithRateLimiter(n uint32) Option {
	return func(c *Client) {
		c.opts = append(c.opts, ttrpc.MaxStreams(n))
	}
}

func WithUnaryClientInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Client) {
		c.interceptor = i
	}
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
	return nil
}

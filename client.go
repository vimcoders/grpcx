package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/generated/api"
	"grpcx/ttrpc"

	"grpcx/encoding"

	"google.golang.org/grpc"
)

type ClientConnInterface interface {
	grpc.ClientConnInterface
	balancer.Picker
}

type Option func(c *Client)

type Client struct {
	balancer.Picker
	encoding.Codec
	interceptor UnaryClientInterceptor
	opts        []ttrpc.Option
}

func DialContext(ctx context.Context, endpoint string, opts ...Option) (ClientConnInterface, error) {
	client := Client{
		Codec: encoding.GetCodec(encoding.Name),
		interceptor: func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
			return rt.RoundTrip(ctx, r)
		},
	}
	for _, o := range opts {
		o(&client)
	}
	picker, err := balancer.DialContext(ctx, endpoint, client.opts...)
	if err != nil {
		return nil, err
	}
	client.Picker = picker
	return &client, nil
}

func WithRateLimiter(n uint32) Option {
	return func(c *Client) {
		c.opts = append(c.opts, ttrpc.MaxStreams(n))
	}
}

func Dial(endpoint string, opts ...Option) (ClientConnInterface, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.Marshal(req)
	if err != nil {
		return err
	}
	info := balancer.PickInfo{
		FullMethodName: method,
	}
	request := &api.Request{
		Method:  method,
		Payload: payload,
	}
	rt, err := c.Pick(ctx, info)
	if err != nil {
		return err
	}
	response, err := c.interceptor(ctx, request, rt)
	if err != nil {
		return err
	}
	if err = c.Unmarshal(response.Payload, reply); err != nil {
		return err
	}
	return nil
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

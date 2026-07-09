package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/generated/api"
	"grpcx/metadata"
	"grpcx/ttrpc"

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type ClientConnInterface interface {
	grpc.ClientConnInterface
	ttrpc.RoundTripper
}

type Option func(c *Client)

type Client struct {
	balancer.Picker
	encoding.Codec
	interceptor UnaryClientInterceptor
	opts        []ttrpc.Option
}

func DialContext(ctx context.Context, endpoint string, opts ...Option) (ClientConnInterface, error) {
	c := Client{
		Codec: encoding.GetCodec(encoding.Name),
		interceptor: func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
			return rt.RoundTrip(ctx, r)
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

func RetriesUnaryClientInterceptor(retries int32) UnaryClientInterceptor {
	return func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (reply *api.Response, err error) {
		for range retries {
			reply, err = rt.RoundTrip(ctx, r)
			if err != nil {
				continue
			}
			switch reply.Code {
			case int32(codes.OK):
				return reply, nil
			case int32(codes.Unavailable):
				fallthrough
			case int32(codes.DeadlineExceeded):
				fallthrough
			case int32(codes.Internal):
				continue
			default:
				return reply, status.Error(codes.Code(reply.Code), reply.Message)
			}
		}
		return reply, err
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
	if medatas, ok := metadata.GetMetadata(ctx); ok {
		request.Metadatas = medatas
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

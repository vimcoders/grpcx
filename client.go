package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/generated/api"
	"grpcx/ttrpc"
	"net"

	"grpcx/encoding"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Option func(c *Client)

type Client struct {
	balancer.Picker
	encoding.Codec
	idle        int32
	retries     int32
	interceptor UnaryClientInterceptor
	opts        []ttrpc.Option
}

func DialContext(ctx context.Context, target string, opts ...Option) (grpc.ClientConnInterface, error) {
	client := Client{
		idle:    1,
		retries: 3,
		Codec:   encoding.GetCodec(encoding.Name),
		interceptor: func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
			return rt.RoundTrip(ctx, r)
		},
	}
	for _, o := range opts {
		o(&client)
	}
	var rts []ttrpc.RoundTripper
	for range client.idle {
		d := net.Dialer{}
		cc, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, err
		}
		rts = append(rts, ttrpc.NewRoundTripper(cc, client.opts...))
	}
	client.Picker = balancer.NewRoundRobinPicker(rts...)
	return &client, nil
}

func WithMaxStreams(n uint32) Option {
	return func(c *Client) {
		c.opts = append(c.opts, ttrpc.MaxStreams(n))
	}
}

func Dial(target string, opts ...Option) (grpc.ClientConnInterface, error) {
	return DialContext(context.Background(), target, opts...)
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
	for range c.retries {
		rt, err := c.Pick(ctx, info)
		if err != nil {
			return err
		}
		response, err := c.interceptor(ctx, request, rt)
		if err != nil {
			return err
		}
		switch codes.Code(response.Code) {
		case codes.OK:
			if err = c.Unmarshal(response.Payload, reply); err != nil {
				return err
			}
			return nil
		case codes.Unavailable:
			continue
		default:
			return status.New(codes.Code(response.Code), response.Message).Err()
		}
	}
	return status.New(codes.Unavailable, codes.Unavailable.String()).Err()
}

// NewStream creates a new stream with the given stream descriptor to the
// specified service and method. If not a streaming client, the request object
// may be provided.
func (c *Client) NewStream(ctx context.Context, desc *grpc.StreamDesc, method string, opts ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, nil
}

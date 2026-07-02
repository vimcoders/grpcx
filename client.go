package grpcx

import (
	"context"
	"grpcx/balancer"
	"grpcx/codec"
	"grpcx/driver"
	"grpcx/generated/api"
	"grpcx/transport"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type Option func(c *Client)

type Client struct {
	codec.Codec
	retries     int32
	rts         []driver.RoundTripper
	interceptor UnaryClientInterceptor
}

func DialContext(ctx context.Context, target string, opts ...Option) (grpc.ClientConnInterface, error) {
	client := Client{
		retries: 3,
		interceptor: func(ctx context.Context, r *api.Request, rt driver.RoundTripper) (*api.Response, error) {
			return rt.RoundTrip(ctx, r)
		},
	}
	for _, o := range opts {
		o(&client)
	}
	for range client.retries {
		d := net.Dialer{}
		cc, err := d.DialContext(ctx, "tcp", target)
		if err != nil {
			return nil, err
		}
		client.rts = append(client.rts, transport.NewRoundTripper(cc))
	}
	return &client, nil
}

func Dial(target string, opts ...Option) (grpc.ClientConnInterface, error) {
	return DialContext(context.Background(), target, opts...)
}

func (c *Client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) error {
	payload, err := c.Marshal(req)
	if err != nil {
		return err
	}
	r := balancer.NewRandPicker(c.rts...)
	for range c.retries {
		rt, err := r.Pick(ctx, driver.PickInfo{})
		if err != nil {
			return err
		}
		response, err := c.interceptor(ctx, &api.Request{
			Metas: &api.Metas{
				Path:      method,
				Authority: GetAuthority(ctx),
			},
			Payload: payload,
		}, rt)
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

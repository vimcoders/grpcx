package grpcx

import (
	"context"
	"grpcx/generated/api"
	"grpcx/transport"
)

type UnaryClientInterceptor func(context.Context, *api.Request, transport.RoundTripper) (*api.Response, error)

func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Client) {
		c.interceptor = i
	}
}

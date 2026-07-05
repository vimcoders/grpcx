package grpcx

import (
	"context"
	"grpcx/generated/api"
	"grpcx/ttrpc"
)

type UnaryClientInterceptor func(context.Context, *api.Request, ttrpc.RoundTripper) (*api.Response, error)

func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Client) {
		c.interceptor = i
	}
}

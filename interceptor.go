package grpcx

import (
	"context"
	"grpcx/driver"
	"grpcx/generated/api"
)

type UnaryClientInterceptor func(context.Context, *api.Request, driver.RoundTripper) (*api.Response, error)

func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Client) {
		c.interceptor = i
	}
}

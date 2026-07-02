package ttrpc

import (
	"context"
	"ttrpc/generated/api"
)

type UnaryClientInterceptor func(context.Context, *api.Request, RoundTripper) (*api.Response, error)

func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Transport) {
		c.interceptor = i
	}
}

package ttrpc

import (
	"context"
	"ttrpc/generated/api"
)

type Invoker func(context.Context, *api.Request) (*api.Response, error)

type UnaryClientInterceptor func(context.Context, *api.Request, Invoker) (*api.Response, error)

func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
	return func(c *Transport) {
		c.interceptor = i
	}
}

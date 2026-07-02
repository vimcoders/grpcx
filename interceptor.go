package ttrpc

import (
	"context"
	"ttrpc/driver"
	"ttrpc/generated/api"
)

type UnaryClientInterceptor func(context.Context, *api.Request, driver.RoundTripper) (*api.Response, error)

// func WithUnaryInterceptor(i UnaryClientInterceptor) Option {
// 	return func(c *Transport) {
// 		c.interceptor = i
// 	}
// }

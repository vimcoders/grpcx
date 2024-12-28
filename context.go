package grpcx

import (
	"context"
)

type OpenTracing struct {
	High   uint64
	Low    uint64
	SpanID uint64
}

type Context struct {
	context.Context
	OpenTracing
}

func WithContext(ctx context.Context, opentracing OpenTracing) context.Context {
	return &Context{Context: ctx, OpenTracing: opentracing}
}

func (x Context) Value(_ any) any {
	return x.OpenTracing
}

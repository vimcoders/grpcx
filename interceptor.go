package grpcx

import (
	"context"
	"grpcx/ttrpc"

	"google.golang.org/grpc"
)

type UnaryClientInterceptor func(ctx context.Context, method string, req any, reply any, rt ttrpc.RoundTripper, opts ...grpc.CallOption) error

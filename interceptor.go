package grpcx

import (
	"context"

	"github.com/vimcoders/grpcx/roundtrip"

	"google.golang.org/grpc"
)

type UnaryClientInterceptor func(ctx context.Context, method string, req any, reply any, rt roundtrip.RoundTripper, opts ...grpc.CallOption) error

package grpcx

import (
	"context"

	"github.com/vimcoders/grpcx/balancer"

	"google.golang.org/grpc"
)

// UnaryClientInterceptor runs around a unary call. It receives the request and
// the reply by reference so it can mutate either, plus the balancer picker the
// connection was built from, and returns the error the caller sees.
type UnaryClientInterceptor func(ctx context.Context, method string, req any, reply any, balance balancer.Picker, opts ...grpc.CallOption) error

// StreamClientInterceptor runs around a streaming call. grpcx serves unary calls
// only, so a registered interceptor of this kind is never invoked.
type StreamClientInterceptor func(ctx context.Context, desc *grpc.StreamDesc, balance balancer.Picker, method string, opts ...grpc.CallOption) (grpc.ClientStream, error)

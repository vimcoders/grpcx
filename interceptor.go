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

// StreamClientInterceptor is the streaming counterpart of
// UnaryClientInterceptor. grpcx carries unary calls only and has no option to
// install one, so the type is kept for source compatibility and is never
// invoked.
type StreamClientInterceptor func(ctx context.Context, desc *grpc.StreamDesc, balance balancer.Picker, method string, opts ...grpc.CallOption) (grpc.ClientStream, error)

package grpcx

import (
	"context"

	"github.com/vimcoders/grpcx/balancer"

	"google.golang.org/grpc"
)

type UnaryClientInterceptor func(ctx context.Context, method string, req any, reply any, balance balancer.Picker, opts ...grpc.CallOption) error
type StreamClientInterceptor func(ctx context.Context, desc *grpc.StreamDesc, balance balancer.Picker, method string, opts ...grpc.CallOption) (grpc.ClientStream, error)

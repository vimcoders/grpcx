package grpcx

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type ClientParameters = keepalive.ClientParameters
type UnaryServerInterceptor = grpc.UnaryServerInterceptor

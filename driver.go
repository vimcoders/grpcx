package grpcx

import (
	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

const _MESSAGE_HEADER = 2

type ClientParameters = keepalive.ClientParameters
type UnaryServerInterceptor = grpc.UnaryServerInterceptor

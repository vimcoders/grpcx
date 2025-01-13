package grpcx

import (
	"context"
	"net"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type ClientParameters = keepalive.ClientParameters
type UnaryServerInterceptor = grpc.UnaryServerInterceptor

type Conn interface {
	net.Addr
	grpc.ClientConnInterface
	Ping(context.Context) error
}

type Handler interface {
	Handle(context.Context, net.Conn) error
}

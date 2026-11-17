package main

import (
	"context"
	"net"
	"os"

	"github.com/vimcoders/grpcx/cmd/internal/stats"
	"github.com/vimcoders/grpcx/generated/api"
	"google.golang.org/grpc"
)

type StdHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *StdHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

func main() {
	stats.Serve()
	var options []grpc.ServerOption
	if stats.Enabled() {
		options = append(options, grpc.ChainUnaryInterceptor(stats.UnaryInterceptor()))
	}

	stdGRPCServer := grpc.NewServer(options...)
	api.RegisterEchoServiceServer(stdGRPCServer, &StdHandler{})
	lis, err := net.Listen("tcp", os.Getenv("PORT"))
	if err != nil {
		panic(err)
	}
	if err := stdGRPCServer.Serve(lis); err != nil {
		panic(err)
	}
}

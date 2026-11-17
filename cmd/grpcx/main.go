package main

import (
	"context"
	"os"

	"github.com/vimcoders/grpcx"
	"github.com/vimcoders/grpcx/cmd/internal/stats"
	"github.com/vimcoders/grpcx/generated/api"
)

type StdHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *StdHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

func main() {
	ttServer := grpcx.NewServer()
	ttServer.RegisterService(&api.EchoService_ServiceDesc, &StdHandler{})

	stats.Serve()
	var options []grpcx.ServerOption
	if stats.Enabled() {
		options = append(options, grpcx.UnaryServerInterceptor(stats.UnaryInterceptor()))
	}

	if err := ttServer.ListenAndServe(context.Background(), os.Getenv("PORT"), options...); err != nil {
		panic(err)
	}
}

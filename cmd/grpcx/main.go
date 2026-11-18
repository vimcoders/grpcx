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
	var options []grpcx.ServerOption
	if stats.Enabled() {
		options = append(options, grpcx.UnaryServerInterceptor(stats.UnaryInterceptor()))
	}
	ttServer := grpcx.NewServer(options...)
	ttServer.RegisterService(&api.EchoService_ServiceDesc, &StdHandler{})
	stats.Serve()

	if err := ttServer.ListenAndServe(context.Background(), os.Getenv("PORT")); err != nil {
		panic(err)
	}
}

package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

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
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	stats.Serve()
	var options []grpcx.ServerOption
	if stats.Enabled() {
		options = append(options, grpcx.UnaryServerInterceptor(stats.UnaryInterceptor()))
	}
	ttServer := grpcx.NewServer(options...)
	defer func() { _ = ttServer.Close() }()
	api.RegisterEchoServiceServer(ttServer, &StdHandler{})
	if err := ttServer.ListenAndServe(ctx, os.Getenv("PORT")); err != nil {
		panic(err)
	}
}

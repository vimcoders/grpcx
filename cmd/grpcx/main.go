package main

import (
	"context"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/vimcoders/grpcx"
	"github.com/vimcoders/grpcx/generated/api"
)

type StdHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *StdHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	ttServer := grpcx.NewServer()
	ttServer.RegisterService(&api.EchoService_ServiceDesc, &StdHandler{})
	go func() {
		if err := ttServer.ListenAndServe(context.Background(), os.Getenv("PORT")); err != nil {
			panic(err)
		}
	}()
	slog.Info("running")
	<-ctx.Done()
	slog.Info("shutdown")
}

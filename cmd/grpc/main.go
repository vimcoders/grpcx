package main

import (
	"context"
	"net"
	"os"
	"os/signal"
	"syscall"

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
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	stdGRPCServer := grpc.NewServer()
	api.RegisterEchoServiceServer(stdGRPCServer, &StdHandler{})
	go func() {
		lis, err := net.Listen("tcp", os.Getenv("PORT"))
		if err != nil {
			panic(err)
		}
		if err := stdGRPCServer.Serve(lis); err != nil {
			panic(err)
		}
	}()
	<-ctx.Done()
}

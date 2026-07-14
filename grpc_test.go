package grpcx_test

import (
	"context"
	"net"
	"testing"

	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

func init() {
	lis, err := net.Listen("tcp", stdGRPCAddr)
	if err != nil {
		panic(err)
	}
	stdGRPCServer = grpc.NewServer()
	api.RegisterEchoServiceServer(stdGRPCServer, &StdHandler{})
	go func() {
		if err := stdGRPCServer.Serve(lis); err != nil {
			panic(err)
		}
	}()
}

type StdHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *StdHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

var (
	stdGRPCServer *grpc.Server
	stdGRPCAddr   = "127.0.0.1:50052"
)

func BenchmarkStdGRPC_Echo(b *testing.B) {
	conn, err := grpc.NewClient(stdGRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: "Hello, std gRPC!"}
	ctx := context.Background()
	// 重置计时器，剔除启动、建连耗时
	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Echo(ctx, req)
		if err != nil {
			b.Errorf("echo call failed: %v", err)
			continue
		}
		_ = resp.Message
	}
}

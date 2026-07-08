package grpcx_test

import (
	"context"
	"net"
	"sync"
	"testing"

	"grpcx/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

type StdHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *StdHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

var (
	stdGRPCServer *grpc.Server
	stdGRPCAddr   = "127.0.0.1:50052"
	setupStdGRPC  sync.Once
)

func startStdGRPCServer() {
	setupStdGRPC.Do(func() {
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
	})
}

// cpu: Intel(R) Core(TM) i5-14600KF
// BenchmarkStdGRPC_Echo
// BenchmarkStdGRPC_Echo-4            13398             80192 ns/op
// BenchmarkStdGRPC_Echo-8            15651             71895 ns/op
// BenchmarkStdGRPC_Echo-16           17827             67518 ns/op
// BenchmarkStdGRPC_Echo-20           16614             67318 ns/op
func BenchmarkStdGRPC_Echo(b *testing.B) {
	startStdGRPCServer()
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

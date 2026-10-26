package grpc_test

import (
	"context"
	"strings"
	"testing"

	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
	"google.golang.org/grpc/credentials/insecure"
)

var (
	stdGRPCAddr = "grpc:50052"
)

func Benchmark1K(b *testing.B) {
	conn, err := grpc.NewClient(stdGRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: strings.Repeat("a", 1024)}
	ctx := context.Background()
	// 重置计时器，剔除启动、建连耗时
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.Echo(ctx, req)
			if err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

func Benchmark5K(b *testing.B) {
	conn, err := grpc.NewClient(stdGRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: strings.Repeat("a", 5120)}
	ctx := context.Background()
	// 重置计时器，剔除启动、建连耗时
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.Echo(ctx, req)
			if err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

func Benchmark1024K(b *testing.B) {
	conn, err := grpc.NewClient(stdGRPCAddr,
		grpc.WithTransportCredentials(insecure.NewCredentials()),
	)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	defer conn.Close()

	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: strings.Repeat("a", 1024*1024)}
	ctx := context.Background()
	// 重置计时器，剔除启动、建连耗时
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			_, err := client.Echo(ctx, req)
			if err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

package grpcx_test

import (
	"context"
	"grpcx"
	"grpcx/generated/api"
	"sync"
	"testing"
)

type TTHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *TTHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

var (
	ttServer *grpcx.Server
	ttAddr   = "127.0.0.1:50051"
	setupTT  sync.Once
)

func startTTServer() {
	setupTT.Do(func() {
		h := &TTHandler{}
		ttServer = grpcx.NewServer()
		ttServer.RegisterService(&api.EchoService_ServiceDesc, h)
		go func() {
			if err := ttServer.ListenAndServe(context.Background(), ttAddr); err != nil {
				panic(err)
			}
		}()
	})
}

// go test -bench "^BenchmarkEcho$" -cpu="4,8,16,20" -v
// cpu: Intel(R) Core(TM) i5-14600KF
// BenchmarkEcho
// BenchmarkEcho-4            42422             28080 ns/op
// BenchmarkEcho-8            43660             27479 ns/op
// BenchmarkEcho-16           42249             28007 ns/op
// BenchmarkEcho-20           42492             27629 ns/op
func BenchmarkEcho(b *testing.B) {
	startTTServer()
	conn, err := grpcx.Dial(ttAddr)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: "Hello, grpcx!"}
	ctx := context.Background()
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

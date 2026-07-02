package grpcx_test

import (
	"context"
	grpcx "grpcx"
	ttrpc "grpcx"
	"grpcx/generated/api"
	"log/slog"
	"testing"
)

type Handler struct {
	api.EchoServiceServer
}

func (h *Handler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	slog.Info("Echo", "req", req.Message)
	return &api.EchoResponse{Message: req.Message}, nil
}

func TestMain(m *testing.M) {
	x := grpcx.NewServer()
	x.RegisterService(&api.EchoService_ServiceDesc, &Handler{})
	go x.ListenAndServe(context.Background(), "127.0.0.1:50051")
	m.Run()
}

func TestEcho(t *testing.T) {
	cc, err := ttrpc.Dial("127.0.0.1:50051")
	if err != nil {
		t.Errorf("failed to call Echo: %v", err)
		return
	}
	req := &api.EchoRequest{Message: "Hello, ttrpc!"}
	resp, err := api.NewEchoServiceClient(cc).Echo(context.Background(), req)
	if err != nil {
		t.Errorf("failed to call Echo: %v", err)
		return
	}
	slog.Info("TestEcho", "req", req.Message, "resp", resp.Message)
}

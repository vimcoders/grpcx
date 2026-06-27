package ttrpc_test

import (
	"context"
	"log/slog"
	"net"
	"testing"
	"ttrpc"
	"ttrpc/generated/api"
)

type Handler struct {
	api.EchoServiceServer
}

func (h *Handler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	slog.Info("Echo", "req", req.Message)
	return &api.EchoResponse{Message: req.Message}, nil
}

func TestMain(m *testing.M) {
	x := ttrpc.NewServer()
	x.RegisterService(&api.EchoService_ServiceDesc, &Handler{})
	go x.ListenAndServe(context.Background(), "127.0.0.1:50051")
	m.Run()
}

func TestEcho(t *testing.T) {
	c, err := net.Dial("tcp", "127.0.0.1:50051")
	if err != nil {
		t.Errorf("failed to call Echo: %v", err)
		return
	}
	slog.Info("connected 127.0.0.1:50051")
	client := ttrpc.NewClient(c)
	slog.Info("NewCliented")
	req := &api.EchoRequest{Message: "Hello, ttrpc!"}
	resp := &api.EchoResponse{}
	err = client.Invoke(context.Background(), "/api.EchoService/Echo", req, resp)
	if err != nil {
		t.Errorf("failed to call Echo: %v", err)
		return
	}
	slog.Info("TestEcho", "req", req.Message, "resp", resp.Message)
}

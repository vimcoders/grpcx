package ttrpc_test

import (
	"context"
	"net"
	"testing"
	"ttrpc"
	"ttrpc/generated/api"
)

type Handler struct {
	api.EchoServiceServer
}

func (h *Handler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

func TestMain(m *testing.M) {
	x := ttrpc.NewServer()
	x.RegisterService(&api.EchoService_ServiceDesc, &Handler{})
	go x.ListenAndServe(context.Background(), "localhost:50051")
	m.Run()
}

func TestEcho(t *testing.T) {
	for range 1000 {
		c, err := net.Dial("tcp", "localhost:50051")
		if err != nil {
			continue
		}
		client := ttrpc.NewClient(c)
		req := &api.EchoRequest{Message: "Hello, ttrpc!"}
		resp := &api.EchoResponse{}
		err = client.Invoke(context.Background(), "api.EchoService/Echo", req, resp)
		if err != nil {
			t.Fatalf("failed to call Echo: %v", err)
		}
	}
}

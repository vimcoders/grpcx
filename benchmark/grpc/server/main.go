package main

import (
	"context"
	"fmt"
	"github/vimcoders/grpcx/benchmark/pb"
	"net"
	"runtime"
	"sync"
	"time"

	"google.golang.org/grpc"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(1)
	listener, err := net.Listen("tcp", ":50051")
	if err != nil {
		panic(err)
	}
	s := grpc.NewServer()
	pb.RegisterChatServer(s, MakeHandler())
	if err := s.Serve(listener); err != nil {
		panic(err)
	}
}

type Handler struct {
	sync.RWMutex
	pb.ChatServer
	total int64
	unix  int64
}

// MakeHandler creates a Handler instance
func MakeHandler() *Handler {
	return &Handler{}
}

func (x *Handler) Chat(ctx context.Context, req *pb.ChatRequest) (*pb.ChatResponse, error) {
	x.Lock()
	defer x.Unlock()
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println(time.Now().Format("2006-01-02 15:04:05"), x.total, "request/s", "NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		x.total = 0
		x.unix = time.Now().Unix()
	}
	return &pb.ChatResponse{}, nil
}

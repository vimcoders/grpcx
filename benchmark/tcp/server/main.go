package main

import (
	"context"
	"fmt"
	"github/vimcoders/grpcx"
	"github/vimcoders/grpcx/benchmark/pb"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(1)
	listener, err := net.Listen("tcp", ":28889")
	if err != nil {
		panic(err)
	}
	opts := []grpcx.ServerOption{
		//grpcx.UnaryInterceptor(x.UnaryInterceptor),
		grpcx.WithServiceDesc(pb.Chat_ServiceDesc),
	}
	svr := grpcx.NewServer(MakeHandler(), opts...)
	go svr.ListenAndServe(context.Background(), listener)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
}

type Handler struct {
	pb.ChatServer
	total int64
	unix  int64
	sync.RWMutex
}

type OpenTracing interface {
	GetOpentracing() *pb.Opentracing
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

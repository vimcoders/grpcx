package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"sync"
	"syscall"
	"time"

	"github.com/vimcoders/grpcx"
	"github.com/vimcoders/grpcx/benchmark/pb"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(1)
	addr, err := net.ResolveTCPAddr("tcp", ":28889")
	if err != nil {
		panic(err)
	}
	opts := []grpcx.ServerOption{
		//grpcx.UnaryInterceptor(x.UnaryInterceptor),
		grpcx.WithServiceDesc(pb.Chat_ServiceDesc),
		grpcx.WithServiceAddr(addr),
	}
	go grpcx.ListenAndServe(context.Background(), MakeHandler(), opts...)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	t := time.NewTicker(time.Second)
	defer t.Stop()
	s := debug.GCStats{}
	for {
		select {
		case <-quit:
			return
		case <-t.C:
			debug.ReadGCStats(&s)
			fmt.Printf("gc %d last@%v, PauseTotal %v\n", s.NumGC, s.LastGC, s.PauseTotal)
		}
	}
}

type Handler struct {
	pb.ChatServer
	total int64
	unix  int64
	sync.RWMutex
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

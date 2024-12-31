package main

import (
	"context"
	"fmt"
	"kitex/kitex_gen/goapi"
	"kitex/kitex_gen/goapi/chat"
	"net"
	"runtime"
	"sync"
	"time"

	"github.com/cloudwego/kitex/pkg/klog"
	"github.com/cloudwego/kitex/server"
)

type Handler struct {
	total int64
	unix  int64
	sync.RWMutex
}

func (x *Handler) Chat(ctx context.Context, Req *goapi.ChatRequest) (r *goapi.ChatResponse, err error) {
	x.Lock()
	defer x.Unlock()
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println("kitex", time.Now().Format("2006-01-02 15:04:05"), x.total, "request/s", "NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		x.total = 0
		x.unix = time.Now().Unix()
	}
	return &goapi.ChatResponse{}, nil
}

func MakeHandler() *Handler {
	return &Handler{}
}

func main() {
	klog.Info(runtime.NumCPU())
	runtime.GOMAXPROCS(1)
	addr, err := net.ResolveTCPAddr("tcp", ":27777")
	if err != nil {
		panic(err)
	}
	svr := chat.NewServer(MakeHandler(), server.WithServiceAddr(addr))
	defer svr.Stop()
	if err := svr.Run(); err != nil {
		klog.Error(err)
	}
}

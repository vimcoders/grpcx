package main

import (
	"context"
	"fmt"
	"grpcx"
	"grpcx/benchmark/pb"
	"io"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"
)

func main() {
	fmt.Println(runtime.NumCPU())
	client, err := Dial("quic", "127.0.0.1:28888")
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	for i := 0; i < 100; i++ {
		go client.BenchmarkChat(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	client.Close()
}

type Client struct {
	pb.ChatClient
	total int64
	unix  int64
	io.Closer
	sync.RWMutex
}

func Dial(network, address string) (*Client, error) {
	cc, err := grpcx.Dial(network, address, grpcx.WithDialServiceDesc(pb.Chat_ServiceDesc))
	if err != nil {
		return nil, err
	}
	return &Client{
		ChatClient: pb.NewChatClient(cc),
	}, nil
}

func (x *Client) after(_ context.Context) {
	x.Lock()
	defer x.Unlock()
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println(x.total)
		x.total = 0
		x.unix = time.Now().Unix()
	}
}

func (x *Client) BenchmarkChat(ctx context.Context) {
	for {
		if _, err := x.Chat(ctx, &pb.ChatRequest{Message: "token"}); err != nil {
			fmt.Println(err.Error())
			return
		}
		x.after(ctx)
	}
}

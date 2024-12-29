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
	"syscall"
	"time"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(3)
	cc, err := grpcx.Dial(context.Background(), "tcp", "127.0.0.1:28889", grpcx.WithDialServiceDesc(pb.Chat_ServiceDesc))
	if err != nil {
		panic(err)
	}
	client := &Client{
		ChatClient: pb.NewChatClient(cc),
	}
	for i := 0; i < 100; i++ {
		go client.BenchmarkChat(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(time.Second)
	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
			fmt.Println("NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		}
	}
}

type Client struct {
	pb.ChatClient
	io.Closer
}

func (x *Client) BenchmarkChat(ctx context.Context) {
	var b []byte
	for i := 0; i < 100; i++ {
		b = append(b, []byte("tokentoken")...)
	}
	message := string(b)
	for {
		if _, err := x.Chat(ctx, &pb.ChatRequest{Message: message}); err != nil {
			fmt.Println(err.Error())
			return
		}
	}
}

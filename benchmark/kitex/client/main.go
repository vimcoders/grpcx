package main

import (
	"context"
	"fmt"
	"kitex/kitex_gen/goapi"
	"kitex/kitex_gen/goapi/chat"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/cloudwego/kitex/client"
)

type Client struct {
	chat.Client
}

func (x *Client) BenchmarkChat(ctx context.Context) {
	var b []byte
	for i := 0; i < 100; i++ {
		b = append(b, []byte("tokentoken")...)
	}
	message := string(b)
	for {
		if _, err := x.Chat(ctx, &goapi.ChatRequest{Message: message}); err != nil {
			fmt.Println(err.Error())
			continue
		}
	}
}

func main() {
	runtime.GOMAXPROCS(2)
	cc, err := chat.NewClient("chat", client.WithHostPorts("127.0.0.1:27777"))
	if err != nil {
		panic(err)
	}
	client := Client{Client: cc}
	for i := 0; i < 8000; i++ {
		go client.BenchmarkChat(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
			fmt.Println("NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		}
	}
}

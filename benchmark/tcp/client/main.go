package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"runtime/debug"
	"syscall"
	"time"

	"github.com/vimcoders/grpcx"
	"github.com/vimcoders/grpcx/benchmark/pb"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(4)
	opts := []grpcx.DialOption{
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
	}
	cc, err := grpcx.Dial(context.Background(), opts...)
	if err != nil {
		panic(err)
	}
	client := &Client{
		ChatClient: pb.NewChatClient(cc),
	}
	for i := 0; i < 10000; i++ {
		go client.BenchmarkChat(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	s := debug.GCStats{}
	for {
		select {
		case <-quit:
			return
		case <-ticker.C:
			debug.ReadGCStats(&s)
			fmt.Printf("gc %d last@%v, PauseTotal %v\n", s.NumGC, s.LastGC, s.PauseTotal)
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
	ticker := time.NewTicker(time.Millisecond * 10)
	defer ticker.Stop()
	for range ticker.C {
		//for {
		if _, err := x.Chat(ctx, &pb.ChatRequest{Message: message}); err != nil {
			fmt.Println(err.Error())
			continue
		}
	}
}

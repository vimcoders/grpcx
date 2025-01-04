package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/vimcoders/grpcx/benchmark/pb"

	"github.com/vimcoders/grpcx"

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(3)
	var cfg = jaegercfg.Configuration{
		ServiceName: "grpcx client", // 对其发起请求的的调用链，叫什么服务
		Sampler: &jaegercfg.SamplerConfig{
			Type:  jaeger.SamplerTypeConst,
			Param: 1,
		},
		Reporter: &jaegercfg.ReporterConfig{
			LogSpans:          true,
			CollectorEndpoint: "http://127.0.0.1:14268/api/traces",
		},
	}
	//jLogger := jaegerlog.StdLogger
	tracer, closer, _ := cfg.NewTracer(
	//jaegercfg.Logger(jLogger),
	)
	opts := []grpcx.DialOption{
		grpcx.WithDial("tcp", "127.0.0.1:28889"),
	}
	cc, err := grpcx.Dial(context.Background(), opts...)
	if err != nil {
		panic(err)
	}
	client := &Client{
		Tracer:     tracer,
		ChatClient: pb.NewChatClient(cc),
		Closer:     closer,
	}
	for i := 0; i < 100000; i++ {
		go client.BenchmarkTracing(context.Background())
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

type Client struct {
	opentracing.Tracer
	pb.ChatClient
	io.Closer
}

func (x *Client) BenchmarkTracing(ctx context.Context) {
	for {
		x.tracing(ctx)
	}
}

func (x *Client) tracing(ctx context.Context) {
	span := x.StartSpan("Chat")
	defer span.Finish()
	spanCtx := span.Context().(jaeger.SpanContext)
	opentracing := pb.Opentracing{
		High:   spanCtx.TraceID().High,
		Low:    spanCtx.TraceID().Low,
		SpanID: uint64(spanCtx.SpanID()),
	}
	if _, err := x.Chat(ctx, &pb.ChatRequest{Message: "token", Opentracing: &opentracing}); err != nil {
		fmt.Println(err.Error())
		return
	}
}

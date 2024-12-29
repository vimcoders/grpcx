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

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
)

func main() {
	fmt.Println(runtime.NumCPU())
	client, err := Dial("tcp", "127.0.0.1:28889")
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	for i := 0; i < 1; i++ {
		go client.BenchmarkTracing(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
	client.Close()
}

type Client struct {
	opentracing.Tracer
	pb.ChatClient
	total int64
	unix  int64
	io.Closer
}

func Dial(network, address string) (*Client, error) {
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
	cc, err := grpcx.Dial("tcp", "127.0.0.1:28889", grpcx.WithDialServiceDesc(pb.Chat_ServiceDesc))
	if err != nil {
		return nil, err
	}
	return &Client{
		ChatClient: pb.NewChatClient(cc),
		Tracer:     tracer,
		Closer:     closer,
	}, nil
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
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println(x.total)
		x.total = 0
		x.unix = time.Now().Unix()
	}
}

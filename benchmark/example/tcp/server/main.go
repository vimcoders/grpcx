package main

import (
	"context"
	"fmt"
	"grpcx"
	"grpcx/benchmark/pb"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"syscall"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"google.golang.org/grpc"
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
	opentracing.Tracer
	io.Closer
	total int64
	unix  int64
	sync.RWMutex
}

type OpenTracing interface {
	GetOpentracing() *pb.Opentracing
}

func (x *Handler) UnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	opentracingAgrs, ok := req.(OpenTracing)
	if !ok {
		return handler(ctx, req)
	}
	t := opentracingAgrs.GetOpentracing()
	if t == nil {
		return handler(ctx, req)
	}
	traceID := jaeger.TraceID{High: t.High, Low: t.Low}
	spanID := jaeger.SpanID(t.SpanID + 1)
	parentID := jaeger.SpanID(t.SpanID)
	spanCtx := jaeger.NewSpanContext(traceID, spanID, parentID, true, nil)
	span := x.StartSpan(info.FullMethod, opentracing.ChildOf(spanCtx))
	defer span.Finish()
	return handler(ctx, req)
}

// MakeHandler creates a Handler instance
func MakeHandler() *Handler {
	var cfg = jaegercfg.Configuration{
		ServiceName: "grpcx server", // 对其发起请求的的调用链，叫什么服务
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
	return &Handler{Tracer: tracer, Closer: closer}
}

func (x *Handler) Chat(ctx context.Context, req *pb.ChatRequest) (*pb.ChatResponse, error) {
	x.Lock()
	defer x.Unlock()
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println(x.total, "request/s", "NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		x.total = 0
		x.unix = time.Now().Unix()
	}
	return &pb.ChatResponse{}, nil
}

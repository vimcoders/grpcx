package main

import (
	"context"
	"fmt"
	"grpcx"
	"grpcx/pb"
	"io"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	"google.golang.org/grpc"

	jaegercfg "github.com/uber/jaeger-client-go/config"
)

func main() {
	fmt.Println(runtime.NumCPU())
	listener, err := net.Listen("tcp", ":28889")
	if err != nil {
		panic(err)
	}
	x := MakeHandler()
	go grpcx.ListenAndServe(context.Background(), listener, x)

	cc, err := grpcx.Dial("tcp", "127.0.0.1:28889", grpcx.WithDialServiceDesc(pb.Parkour_ServiceDesc))
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	client := pb.NewParkourClient(cc)
	benchmarkClient := Client{
		ParkourClient: client,
		Tracer:        x.Tracer,
	}
	for i := 0; i < 2; i++ {
		go benchmarkClient.BenchmarkLogin(context.Background())
	}
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
}

type Handler struct {
	pb.ParkourServer
	opentracing.Tracer
	io.Closer
}

func (x Handler) UnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
	t, ok := ctx.Value("opentracing").(grpcx.OpenTracing)
	if !ok {
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
		ServiceName: "grpcx test", // 对其发起请求的的调用链，叫什么服务
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

func (x *Handler) Handle(ctx context.Context, conn net.Conn) {
	svr := grpcx.NewServer( /*grpcx.UnaryInterceptor(x.UnaryInterceptor)*/ )
	svr.RegisterService(&pb.Parkour_ServiceDesc, x)
	go svr.Serve(ctx, conn)
}

func (x *Handler) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	return &pb.LoginResponse{}, nil
}

type Client struct {
	opentracing.Tracer
	pb.ParkourClient
	total int64
	unix  int64
}

func (x *Client) BenchmarkLogin(ctx context.Context) {
	for {
		// span := x.StartSpan("Login")
		// spanCtx := span.Context().(jaeger.SpanContext)
		opentracing := grpcx.OpenTracing{
			// High:   spanCtx.TraceID().High,
			// Low:    spanCtx.TraceID().Low,
			// SpanID: uint64(spanCtx.SpanID()),
		}
		ctx := grpcx.WithContext(context.Background(), opentracing)
		if _, err := x.Login(ctx, &pb.LoginRequest{Token: "token"}); err != nil {
			fmt.Println(err.Error())
			return
		}
		//span.Finish()
		x.total++
		if x.unix != time.Now().Unix() {
			fmt.Println(x.total)
			x.total = 0
			x.unix = time.Now().Unix()
		}
	}
}

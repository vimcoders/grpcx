package grpcx_test

import (
	"context"
	"runtime"
	"strings"
	"testing"

	"github.com/vimcoders/grpcx/metadata"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
)

var (
	ttAddr = "grpcx:50051"
)

func Benchmark1K(b *testing.B) {
	// WithBufSize 高并发下开启批量发送，默认不开启
	numProcs := runtime.GOMAXPROCS(0)
	c, err := grpcx.Dial(ttAddr, grpcx.WithBufSize(numProcs*1024))
	if err != nil {
		b.Errorf("Dial failed: %v", err)
		return
	}
	defer c.Close()
	client := api.NewEchoServiceClient(c)
	req := &api.EchoRequest{Message: strings.Repeat("a", 1024)}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := client.Echo(ctx, req); err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

func Benchmark5K(b *testing.B) {
	// WithBufSize 高并发下开启批量发送，默认不开启
	numProcs := runtime.GOMAXPROCS(0)
	c, err := grpcx.Dial(ttAddr, grpcx.WithBufSize(numProcs*1024))
	if err != nil {
		b.Errorf("Dial failed: %v", err)
		return
	}
	defer c.Close()
	client := api.NewEchoServiceClient(c)
	req := &api.EchoRequest{Message: strings.Repeat("a", 5120)}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := client.Echo(ctx, req); err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

func Benchmark1024K(b *testing.B) {
	// WithBufSize 高并发下开启批量发送，默认不开启
	numProcs := runtime.GOMAXPROCS(0)
	c, err := grpcx.Dial(ttAddr, grpcx.WithBufSize(numProcs*1024))
	if err != nil {
		b.Errorf("Dial failed: %v", err)
		return
	}
	defer c.Close()
	client := api.NewEchoServiceClient(c)
	req := &api.EchoRequest{Message: strings.Repeat("a", 1024*1024)}
	ctx := context.Background()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			if _, err := client.Echo(ctx, req); err != nil {
				b.Errorf("echo call failed: %v", err)
			}
		}
	})
}

func OtelUnaryServerInterceptor() grpc.UnaryServerInterceptor {
	var tracer = otel.Tracer("grpc-client-retries")
	var propagator = otel.GetTextMapPropagator()
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		if md, ok := metadata.GetMetadata(ctx); ok {
			carrier := propagation.MapCarrier(md)
			childCtx, span := tracer.Start(propagator.Extract(ctx, carrier), "ttrpc.server.handle",
				trace.WithAttributes(
					semconv.RPCSystemKey.String("ttrpc"),
					semconv.RPCMethodKey.String(info.FullMethod),
				),
			)
			defer span.End()
			resp, err := handler(childCtx, req)
			if err != nil {
				span.SetStatus(otelcodes.Error, err.Error())
				span.RecordError(err)
			}
			return resp, err
		}
		return handler(ctx, req)
	}
}

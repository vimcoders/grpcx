package grpcx_test

import (
	"context"
	"testing"

	"github.com/vimcoders/grpcx/balancer"
	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/metadata"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

var (
	ttAddr = "127.0.0.1:50051"
)

func Benchmark(b *testing.B) {
	c, err := grpcx.Dial(ttAddr)
	if err != nil {
		b.Errorf("Dial failed: %v", err)
		return
	}
	defer c.Close()
	client := api.NewEchoServiceClient(c)
	req := &api.EchoRequest{Message: "Hello, grpcx!"}
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		if _, err := client.Echo(ctx, req); err != nil {
			b.Errorf("echo call failed: %v", err)
		}
	}
}

func BenchmarkParallel(b *testing.B) {
	c, err := grpcx.Dial(ttAddr)
	if err != nil {
		b.Errorf("Dial failed: %v", err)
		return
	}
	defer c.Close()
	client := api.NewEchoServiceClient(c)
	req := &api.EchoRequest{Message: "Hello, grpcx!"}
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

func RetriesUnaryClientInterceptor(retries int32) grpcx.UnaryClientInterceptor {
	return func(ctx context.Context, method string, req any, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
		for i := retries; i >= 0; i-- {
			rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
			if err != nil {
				return err
			}
			if err := rt.Invoke(ctx, method, req, reply, opts...); err != nil {
				if s, ok := status.FromError(err); ok {
					switch s.Code() {
					case codes.Unavailable:
						fallthrough
					case codes.Internal:
						fallthrough
					case codes.DeadlineExceeded:
						continue
					default:
						return err
					}
				}
				return err
			}
			return nil
		}
		return status.OutOfRange.Err()
	}
}

func OtelUnaryClientInterceptor() grpcx.UnaryClientInterceptor {
	var tracer = otel.Tracer("grpc-client-retries")
	var propagator = otel.GetTextMapPropagator()
	return func(ctx context.Context, method string, req any, reply any, balance balancer.Picker, opts ...grpc.CallOption) error {
		rt, err := balance.Pick(ctx, balancer.PickInfo{FullMethodName: method})
		if err != nil {
			return err
		}
		otelCtx, span := tracer.Start(ctx, "ttrpc.client.call",
			trace.WithAttributes(
				semconv.RPCSystemKey.String("ttrpc"),
				semconv.RPCMethodKey.String(method),
			),
		)
		defer span.End()
		carrier := propagation.MapCarrier(make(map[string]string))
		propagator.Inject(otelCtx, carrier)
		var md []string
		for k, v := range carrier {
			md = append(md, k, v)
		}
		if err := rt.Invoke(metadata.AppendToContext(otelCtx, md...), method, req, reply, opts...); err != nil {
			span.SetStatus(otelcodes.Error, err.Error())
			span.RecordError(err)
			return err
		}
		return nil
	}
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

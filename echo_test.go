package grpcx_test

import (
	"context"
	"grpcx"
	"grpcx/generated/api"
	"grpcx/metadata"
	"grpcx/status"
	"grpcx/ttrpc"
	"sync"
	"testing"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	semconv "go.opentelemetry.io/otel/semconv/v1.37.0"
	"go.opentelemetry.io/otel/trace"
	"google.golang.org/grpc/codes"
)

type TTHandler struct {
	api.UnimplementedEchoServiceServer
}

func (h *TTHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

var (
	ttServer *grpcx.Server
	ttAddr   = "127.0.0.1:50051"
	setupTT  sync.Once
)

func startTTServer() {
	setupTT.Do(func() {
		h := &TTHandler{}
		ttServer = grpcx.NewServer()
		ttServer.RegisterService(&api.EchoService_ServiceDesc, h)
		go func() {
			if err := ttServer.ListenAndServe(context.Background(), ttAddr); err != nil {
				panic(err)
			}
		}()
	})
}

// go test -bench "^BenchmarkEcho$" -cpu="4,8,16,20" -v
// cpu: Intel(R) Core(TM) i5-14600KF
// BenchmarkEcho
// BenchmarkEcho-4            42422             28080 ns/op
// BenchmarkEcho-8            43660             27479 ns/op
// BenchmarkEcho-16           42249             28007 ns/op
// BenchmarkEcho-20           42492             27629 ns/op
func BenchmarkEcho(b *testing.B) {
	startTTServer()
	conn, err := grpcx.Dial(ttAddr)
	if err != nil {
		b.Fatalf("dial failed: %v", err)
	}
	client := api.NewEchoServiceClient(conn)
	req := &api.EchoRequest{Message: "Hello, grpcx!"}
	ctx := context.Background()
	b.ResetTimer()
	for b.Loop() {
		resp, err := client.Echo(ctx, req)
		if err != nil {
			b.Errorf("echo call failed: %v", err)
			continue
		}
		_ = resp.Message
	}
}

func RetriesUnaryClientInterceptor(retries int32) grpcx.UnaryClientInterceptor {
	return func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
		for i := retries; i >= 0; i-- {
			reply, err := rt.RoundTrip(ctx, r)
			if err != nil {
				return reply, err
			}
			if i == 0 {
				return reply, nil
			}
			code := codes.Code(reply.Code)
			switch code {
			case codes.OK:
				return reply, nil
			case codes.Unavailable:
				fallthrough
			case codes.DeadlineExceeded:
				fallthrough
			case codes.Internal:
				continue
			default:
				return reply, status.Error(code, reply.Message)
			}
		}
		return nil, status.OutOfRange.Err()
	}
}

func OtelUnaryClientInterceptor(retries int32) grpcx.UnaryClientInterceptor {
	tracer := otel.Tracer("grpc-client-retries")
	var propagator = otel.GetTextMapPropagator()
	return func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
		otelCtx, span := tracer.Start(ctx, "ttrpc.client.call",
			trace.WithAttributes(
				semconv.RPCSystemKey.String("ttrpc"),
				semconv.RPCMethodKey.String(r.Method),
			),
		)
		defer span.End()
		carrier := propagation.MapCarrier(make(map[string]string))
		propagator.Inject(otelCtx, carrier)
		var md metadata.MD
		for k, v := range carrier {
			md = append(md, k, v)
		}
		return rt.RoundTrip(metadata.AppendToContext(ctx, md...), r)
	}
}

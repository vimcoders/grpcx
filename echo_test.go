package grpcx_test

import (
	"context"
	"grpcx"
	"grpcx/encoding"
	"grpcx/generated/api"
	"grpcx/transport"
	"path"
	"sync"
	"testing"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

type TTHandler struct {
	api.UnimplementedEchoServiceServer
	encoding.Codec
}

func (h *TTHandler) Echo(ctx context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	return &api.EchoResponse{Message: req.Message}, nil
}

func (h *TTHandler) Interceptor(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
	return handler(ctx, req)
}

func (h *TTHandler) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	for _, v := range api.EchoService_ServiceDesc.Methods {
		path := path.Join("/", api.EchoService_ServiceDesc.ServiceName, v.MethodName)
		if path != req.Method {
			continue
		}
		reply, err := v.Handler(h, ctx, func(in any) error {
			return h.Unmarshal(req.Payload, in)
		}, h.Interceptor)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		response, err := h.Marshal(reply)
		if err != nil {
			return &api.Response{
				Code:    int32(codes.Unavailable),
				Message: err.Error(),
			}, nil
		}
		return &api.Response{
			Code:    int32(codes.OK),
			Message: codes.OK.String(),
			Payload: response,
		}, nil
	}
	return &api.Response{
		Code:    int32(codes.Unimplemented),
		Message: codes.Unimplemented.String(),
	}, nil
}

var (
	ttServer *grpcx.Server
	ttAddr   = "127.0.0.1:50051"
	setupTT  sync.Once
)

func startTTServer() {
	setupTT.Do(func() {
		h := &TTHandler{
			Codec: encoding.GetCodec(encoding.Name),
		}
		opts := []transport.ServerOption{
			transport.UnaryServerInterceptor(h),
		}
		ttServer = grpcx.NewServer(opts...)
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

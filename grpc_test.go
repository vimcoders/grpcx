package grpcx_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"grpcx"
	"grpcx/pb"
	"io"
	"math/big"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	jaegerlog "github.com/uber/jaeger-client-go/log"
	"google.golang.org/grpc"
)

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
	time.Sleep(time.Second)
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
	jLogger := jaegerlog.StdLogger
	tracer, closer, _ := cfg.NewTracer(
		jaegercfg.Logger(jLogger),
	)
	return &Handler{Tracer: tracer, Closer: closer}
}

func (x *Handler) Handle(ctx context.Context, conn net.Conn) {
	svr := grpcx.NewServer(grpcx.UnaryInterceptor(x.UnaryInterceptor))
	svr.RegisterService(&pb.Parkour_ServiceDesc, x)
	go svr.Serve(ctx, conn)
}

func (x *Handler) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	fmt.Println("Login")
	return &pb.LoginResponse{}, nil
}

func TestMain(m *testing.M) {
	// listener, err := quicx.Listen("udp", ":28889", GenerateTLSConfig(), &quicx.Config{
	// 	MaxIdleTimeout: time.Minute,
	// })
	fmt.Println(runtime.NumCPU())
	listener, err := net.Listen("tcp", ":28889")
	if err != nil {
		panic(err)
	}
	x := MakeHandler()
	go grpcx.ListenAndServe(context.Background(), listener, x)
	m.Run()
	x.Close()
}

func GenerateTLSConfig() *tls.Config {
	key, err := rsa.GenerateKey(rand.Reader, 1024)
	if err != nil {
		panic(err)
	}
	template := x509.Certificate{SerialNumber: big.NewInt(1)}
	certDER, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		panic(err)
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: certDER})

	tlsCert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		panic(err)
	}
	return &tls.Config{
		Certificates: []tls.Certificate{tlsCert},
		NextProtos:   []string{"quic-echo-example"},
		MaxVersion:   tls.VersionTLS13,
	}
}

func TestQUIC(t *testing.T) {
	t.Log(runtime.NumCPU())
	var cfg = jaegercfg.Configuration{
		ServiceName: "balance test", // 对其发起请求的的调用链，叫什么服务
		Sampler: &jaegercfg.SamplerConfig{
			Type:  jaeger.SamplerTypeConst,
			Param: 1,
		},
		Reporter: &jaegercfg.ReporterConfig{
			LogSpans:          true,
			CollectorEndpoint: "http://127.0.0.1:14268/api/traces",
		},
	}
	jLogger := jaegerlog.StdLogger
	tracer, closer, _ := cfg.NewTracer(
		jaegercfg.Logger(jLogger),
	)
	defer closer.Close()
	cc, err := grpcx.Dial("tcp", "127.0.0.1:28889", grpcx.WithDialServiceDesc(pb.Parkour_ServiceDesc))
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	client := pb.NewParkourClient(cc)
	span := tracer.StartSpan("Login")
	defer span.Finish()
	spanCtx := span.Context().(jaeger.SpanContext)
	opentracing := grpcx.OpenTracing{
		High:   spanCtx.TraceID().High,
		Low:    spanCtx.TraceID().Low,
		SpanID: uint64(spanCtx.SpanID()),
	}
	ctx := grpcx.WithContext(context.Background(), opentracing)
	if _, err := client.Login(ctx, &pb.LoginRequest{Token: "token"}); err != nil {
		fmt.Println(err.Error())
		return
	}
	time.Sleep(time.Second)
}

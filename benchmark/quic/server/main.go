package main

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"grpcx"
	"grpcx/benchmark/pb"
	"io"
	"math/big"
	"net"
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/opentracing/opentracing-go"
	"github.com/uber/jaeger-client-go"
	jaegercfg "github.com/uber/jaeger-client-go/config"
	"github.com/vimcoders/quicx"
	"google.golang.org/grpc"
)

func main() {
	fmt.Println(runtime.NumCPU())
	listener, err := quicx.Listen("udp", ":28888", GenerateTLSConfig(), &quicx.Config{
		MaxIdleTimeout: time.Minute,
	})
	if err != nil {
		panic(err)
	}
	x := MakeHandler()
	go grpcx.ListenAndServe(context.Background(), listener, x)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
}

type Handler struct {
	pb.ChatServer
	opentracing.Tracer
	io.Closer
}

type OpenTracing interface {
	GetOpentracing() *pb.Opentracing
}

func (x Handler) UnaryInterceptor(ctx context.Context, req interface{}, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (interface{}, error) {
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

func (x *Handler) Handle(ctx context.Context, conn net.Conn) {
	svr := grpcx.NewServer(grpcx.UnaryInterceptor(x.UnaryInterceptor))
	svr.RegisterService(&pb.Chat_ServiceDesc, x)
	go svr.Serve(ctx, conn)
}

func (x *Handler) Chat(ctx context.Context, req *pb.ChatRequest) (*pb.ChatResponse, error) {
	return &pb.ChatResponse{}, nil
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
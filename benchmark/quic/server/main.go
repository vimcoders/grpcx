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
	"os"
	"os/signal"
	"runtime"
	"syscall"
	"time"

	"github.com/vimcoders/quicx"
)

func main() {
	fmt.Println(runtime.NumCPU())
	runtime.GOMAXPROCS(1)
	listener, err := quicx.Listen("udp", ":28888", GenerateTLSConfig(), &quicx.Config{
		MaxIdleTimeout: time.Minute,
	})
	if err != nil {
		panic(err)
	}
	x := MakeHandler()
	opts := []grpcx.ServerOption{
		//grpcx.UnaryInterceptor(x.UnaryInterceptor),
		grpcx.WithServiceDesc(pb.Chat_ServiceDesc),
	}
	svr := grpcx.NewServer(x, opts...)
	go svr.ListenAndServe(context.Background(), listener)
	quit := make(chan os.Signal, 1)
	signal.Notify(quit, syscall.SIGHUP, syscall.SIGQUIT, syscall.SIGTERM, syscall.SIGINT)
	<-quit
}

type Handler struct {
	pb.ChatServer
	io.Closer
	total int64
	unix  int64
}

type OpenTracing interface {
	GetOpentracing() *pb.Opentracing
}

// MakeHandler creates a Handler instance
func MakeHandler() *Handler {
	return &Handler{}
}

func (x *Handler) Chat(ctx context.Context, req *pb.ChatRequest) (*pb.ChatResponse, error) {
	x.total++
	if x.unix != time.Now().Unix() {
		fmt.Println(x.total, "request/s", "NumCPU:", runtime.NumCPU(), "NumGoroutine:", runtime.NumGoroutine())
		x.total = 0
		x.unix = time.Now().Unix()
	}
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

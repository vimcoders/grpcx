package grpcx_test

import (
	"balance/grpcx"
	"balance/pb"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"encoding/pem"
	"fmt"
	"math/big"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/vimcoders/go-driver/quicx"
)

type Handler struct {
	pb.ParkourServer
}

// MakeHandler creates a Handler instance
func MakeHandler() *Handler {
	return &Handler{}
}

func (x *Handler) Handle(ctx context.Context, conn net.Conn) {
	svr := grpcx.NewServer()
	svr.RegisterService(&pb.Parkour_ServiceDesc, x)
	go svr.Serve(ctx, conn)
}

func (x *Handler) Login(ctx context.Context, req *pb.LoginRequest) (*pb.LoginResponse, error) {
	fmt.Println("Login")
	return &pb.LoginResponse{}, nil
}

func (x *Handler) Close() error {
	return nil
}

func TestMain(m *testing.M) {
	qlistener, err := quicx.Listen("udp", ":28889", GenerateTLSConfig(), &quicx.Config{
		MaxIdleTimeout: time.Minute,
	})
	if err != nil {
		panic(err)
	}
	go grpcx.ListenAndServe(context.Background(), qlistener, MakeHandler())
	m.Run()
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

func BenchmarkQUIC(b *testing.B) {
	fmt.Println(runtime.NumCPU())
	cc, err := grpcx.Dial("udp", "127.0.0.1:28889", grpcx.WithDialServiceDesc(pb.Parkour_ServiceDesc))
	if err != nil {
		fmt.Println(err.Error())
		return
	}
	client := pb.NewParkourClient(cc)
	for i := 0; i < b.N; i++ {
		if _, err := client.Login(context.Background(), &pb.LoginRequest{Token: "token"}); err != nil {
			fmt.Println(err.Error())
			return
		}
	}
}

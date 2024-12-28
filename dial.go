package grpcx

import (
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/vimcoders/go-driver/quicx"
	"google.golang.org/grpc"
)

type DialOption interface {
	apply(*Option)
}

type funcDialOption struct {
	f func(*Option)
}

func (x *funcDialOption) apply(o *Option) {
	x.f(o)
}

func WithDialServiceDesc(info grpc.ServiceDesc) DialOption {
	return newFuncDialOption(func(o *Option) {
		o.Methods = info.Methods
	})
}

func newFuncDialOption(f func(*Option)) *funcDialOption {
	return &funcDialOption{
		f: f,
	}
}

func Dial(network string, addr string, opts ...DialOption) (Client, error) {
	opt := Option{
		buffsize: 1024,
		timeout:  time.Minute,
	}
	for i := 0; i < len(opts); i++ {
		opts[i].apply(&opt)
	}
	switch network {
	case "udp":
		conn, err := quicx.Dial(addr, &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-echo-example"},
			MaxVersion:         tls.VersionTLS13,
		}, &quicx.Config{
			MaxIdleTimeout: time.Minute,
		})
		if err != nil {
			return nil, err
		}
		return newClient(conn, opt), nil
	case "tcp":
		fallthrough
	case "tcp4":
		conn, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		return newClient(conn, opt), nil
	}
	return nil, fmt.Errorf("%s unkonw", network)
}

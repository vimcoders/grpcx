package grpcx

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"time"

	"github.com/vimcoders/quicx"
	"google.golang.org/grpc"
)

type DialOption interface {
	apply(*clientOption)
}

type funcDialOption struct {
	f func(*clientOption)
}

func (x *funcDialOption) apply(o *clientOption) {
	x.f(o)
}

func WithClientDial(network, address string) DialOption {
	return newFuncDialOption(func(o *clientOption) {
		o.network = network
		o.address = address
	})
}

func WithDialServiceDesc(info grpc.ServiceDesc) DialOption {
	return newFuncDialOption(func(o *clientOption) {
		for i := 0; i < len(info.Methods); i++ {
			o.Methods = append(o.Methods, info.Methods[i].MethodName)
		}
	})
}

func newFuncDialOption(f func(*clientOption)) *funcDialOption {
	return &funcDialOption{
		f: f,
	}
}

func Dial(ctx context.Context, opts ...DialOption) (grpc.ClientConnInterface, error) {
	opt := defaultClientOptions
	for i := 0; i < len(opts); i++ {
		opts[i].apply(&opt)
	}
	switch opt.network {
	case "quic":
		conn, err := quicx.Dial(opt.address, &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-echo-example"},
			MaxVersion:         tls.VersionTLS13,
		}, &quicx.Config{
			MaxIdleTimeout: time.Minute,
		})
		if err != nil {
			return nil, err
		}
		return newClient(ctx, conn, opt), nil
	case "tcp":
		fallthrough
	case "tcp4":
		conn, err := net.Dial("tcp", opt.address)
		if err != nil {
			return nil, err
		}
		return newClient(ctx, conn, opt), nil
	}
	return nil, fmt.Errorf("%s unkonw", opt.network)
}

package grpcx

import (
	"context"
	"crypto/tls"
	"grpcx/discovery"
	"net"
	"time"

	"github.com/vimcoders/quicx"
	"google.golang.org/grpc"
)

type dialOption struct {
	address  []net.Addr
	buffsize uint16
	timeout  time.Duration
	Methods  []string
}

var defaultDialOptions = dialOption{
	timeout:  120 * time.Second,
	buffsize: defaultReadBufSize,
}

type DialOption interface {
	apply(*dialOption)
}

type funcDialOption struct {
	f func(*dialOption)
}

func (x *funcDialOption) apply(o *dialOption) {
	x.f(o)
}

func WithDial(network, address string) DialOption {
	return newFuncDialOption(func(o *dialOption) {
		o.address = append(o.address, NewAddr(network, address))
	})
}

func WithDialServiceDesc(info grpc.ServiceDesc) DialOption {
	return newFuncDialOption(func(o *dialOption) {
		for i := 0; i < len(info.Methods); i++ {
			o.Methods = append(o.Methods, info.Methods[i].MethodName)
		}
	})
}

func newFuncDialOption(f func(*dialOption)) *funcDialOption {
	return &funcDialOption{
		f: f,
	}
}

func Dial(ctx context.Context, opts ...DialOption) (grpc.ClientConnInterface, error) {
	opt := defaultDialOptions
	for i := 0; i < len(opts); i++ {
		opts[i].apply(&opt)
	}
	clientOpt := clientOption{
		timeout:  opt.timeout,
		buffsize: opt.buffsize,
		Methods:  opt.Methods,
	}
	var client client
	for i := 0; i < len(opt.address); i++ {
		switch opt.address[i].Network() {
		case "tcp":
			conn, err := net.Dial("tcp", opt.address[i].String())
			if err != nil {
				return nil, err
			}
			client.cc = append(client.cc, newClient(ctx, conn, clientOpt))
		case "udp":
			conn, err := quicx.Dial(opt.address[i].String(), &tls.Config{
				InsecureSkipVerify: true,
				NextProtos:         []string{"quic-echo-example"},
				MaxVersion:         tls.VersionTLS13,
			}, &quicx.Config{
				MaxIdleTimeout: time.Minute,
			})
			if err != nil {
				return nil, err
			}
			client.cc = append(client.cc, newClient(ctx, conn, clientOpt))
		}
	}
	for i := 0; i < len(client.cc); i++ {
		client.Instances = append(client.Instances, discovery.NewInstance(client.cc[i], i, nil))
	}
	return &client, nil
}

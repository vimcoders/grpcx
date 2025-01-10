package grpcx

import (
	"context"
	"math"
	"net"
	"time"

	"github.com/vimcoders/grpcx/discovery"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type dialOption struct {
	address         []net.Addr
	buffsize        uint16
	timeout         time.Duration
	Methods         []string
	KeepaliveParams keepalive.ClientParameters
}

var defaultDialOptions = dialOption{
	timeout:         120 * time.Second,
	buffsize:        defaultReadBufSize,
	KeepaliveParams: keepalive.ClientParameters{Time: time.Second * 60, Timeout: time.Second * 60},
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

func WithKeepaliveParams(kp keepalive.ClientParameters) DialOption {
	return newFuncDialOption(func(o *dialOption) {
		o.KeepaliveParams = kp
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
		timeout:         opt.timeout,
		buffsize:        opt.buffsize,
		Methods:         opt.Methods,
		maxRetry:        5,
		KeepaliveParams: opt.KeepaliveParams,
	}
	client := client{clientOption: clientOpt}
	for i := 0; i < len(opt.address); i++ {
		cc, err := dail(ctx, opt.address[i], opts...)
		if err != nil {
			panic(err)
		}
		client.cc = append(client.cc, cc)
		client.Instances = append(client.Instances, discovery.NewInstance(cc, i, nil))
	}
	go client.Keepalive(ctx)
	return &client, nil
}

func dail(ctx context.Context, addr net.Addr, opts ...DialOption) (*conn, error) {
	opt := defaultDialOptions
	for i := 0; i < len(opts); i++ {
		opts[i].apply(&opt)
	}
	clientOpt := clientOption{
		timeout:         opt.timeout,
		buffsize:        opt.buffsize,
		Methods:         opt.Methods,
		maxRetry:        5,
		KeepaliveParams: opt.KeepaliveParams,
	}
	// _dial := func(_addr net.Addr) (net.Conn, error) {
	// 	if _addr.Network() == "tcp" {
	// 		return net.Dial("tcp", _addr.String())
	// 	}
	// 	return quicx.Dial(_addr.String(), &tls.Config{
	// 		InsecureSkipVerify: true,
	// 		NextProtos:         []string{"quic-echo-example"},
	// 		MaxVersion:         tls.VersionTLS13,
	// 	}, &quicx.Config{
	// 		MaxIdleTimeout: time.Minute,
	// 	})
	// }
	c, err := net.Dial("tcp", addr.String())
	if err != nil {
		return nil, err
	}
	x := &conn{
		Conn:         c,
		Context:      ctx,
		clientOption: clientOpt,
		q:            make([]chan []byte, math.MaxUint16),
		ch:           make(chan uint16, math.MaxUint16),
	}
	for i := uint16(0); i < math.MaxUint16; i++ {
		x.ch <- i
		x.q = append(x.q, make(chan []byte, 1))
	}
	go x.serve(ctx)
	if err := x.Ping(ctx); err != nil {
		return nil, err
	}
	return x, nil
}

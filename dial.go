package grpcx

import (
	"context"
	"crypto/tls"
	"math"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/discovery"
	"github.com/vimcoders/grpcx/quicx"

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

// func WithDialServiceDesc(info grpc.ServiceDesc) DialOption {
// 	return newFuncDialOption(func(o *dialOption) {
// 		for i := 0; i < len(info.Methods); i++ {
// 			o.Methods = append(o.Methods, info.Methods[i].MethodName)
// 		}
// 	})
// }

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
	var client client
	for i := 0; i < len(opt.address); i++ {
		cc, err := dail(ctx, opt.address[i].Network(), opt.address[i].String(), opts...)
		if err != nil {
			panic(err)
		}
		go cc.serve(ctx)
		if err := cc.Ping(ctx); err != nil {
			panic(err)
		}
		client.cc = append(client.cc, cc)
		client.Instances = append(client.Instances, discovery.NewInstance(cc, i, nil))
	}
	return &client, nil
}

func dail(ctx context.Context, network string, addr string, opts ...DialOption) (*conn, error) {
	opt := defaultDialOptions
	for i := 0; i < len(opts); i++ {
		opts[i].apply(&opt)
	}
	clientOpt := clientOption{
		timeout:         opt.timeout,
		buffsize:        opt.buffsize,
		Methods:         opt.Methods,
		maxRetry:        5,
		retrySleep:      time.Second * 10,
		KeepaliveParams: opt.KeepaliveParams,
	}
	switch network {
	case "tcp":
		c, err := net.Dial("tcp", addr)
		if err != nil {
			return nil, err
		}
		cancelCtx, cancelFunc := context.WithCancel(ctx)
		x := &conn{
			Conn:         c,
			Context:      cancelCtx,
			CancelFunc:   cancelFunc,
			clientOption: clientOpt,
			pending:      make(map[uint16]request),
			seq:          math.MaxUint8,
		}
		x.Pool = sync.Pool{
			New: x.NewRequest,
		}
		return x, nil
	case "udp":
		c, err := quicx.Dial(addr, &tls.Config{
			InsecureSkipVerify: true,
			NextProtos:         []string{"quic-echo-example"},
			MaxVersion:         tls.VersionTLS13,
		}, &quicx.Config{
			MaxIdleTimeout: time.Minute,
		})
		if err != nil {
			return nil, err
		}
		cancelCtx, cancelFunc := context.WithCancel(ctx)
		x := &conn{
			Conn:         c,
			Context:      cancelCtx,
			CancelFunc:   cancelFunc,
			clientOption: clientOpt,
			pending:      make(map[uint16]request),
			seq:          math.MaxUint8,
		}
		x.Pool = sync.Pool{
			New: x.NewRequest,
		}
		return x, nil
	}
	return nil, nil
}

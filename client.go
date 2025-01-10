package grpcx

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"time"

	"github.com/vimcoders/grpcx/balance"
	"github.com/vimcoders/grpcx/discovery"
	"github.com/vimcoders/grpcx/log"

	"google.golang.org/grpc"
	"google.golang.org/grpc/keepalive"
)

type clientOption struct {
	buffsize        uint16
	timeout         time.Duration
	Methods         []string
	maxRetry        int
	KeepaliveParams keepalive.ClientParameters
}

type client struct {
	clientOption
	cc []Conn
	grpc.ClientConnInterface
	balance.Balancer
	discovery.Result
}

func (x *client) Invoke(ctx context.Context, method string, req any, reply any, opts ...grpc.CallOption) (err error) {
	if len(x.Instances) <= 0 {
		return fmt.Errorf("instance %s not found", method)
	}
	instance := x.GetPicker(x.Result).Next(ctx, req)
	for i := 0; i < len(x.cc); i++ {
		if x.cc[i].Network() != instance.Address().Network() {
			continue
		}
		if x.cc[i].String() != instance.Address().String() {
			continue
		}
		return x.cc[i].Invoke(ctx, method, req, reply, opts...)
	}
	return fmt.Errorf("instance %s not found", instance.Address().String())
}

func (x *client) GetPicker(result discovery.Result) balance.Picker {
	return balance.NewRandomPicker(result.Instances)
}

func (x *client) Keepalive(ctx context.Context) (err error) {
	defer func() {
		if e := recover(); e != nil {
			log.Error(e)
			debug.PrintStack()
		}
		if err != nil {
			log.Error(err)
		}
	}()
	ticker := time.NewTicker(x.KeepaliveParams.Time)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return errors.New("shutdown")
		case <-ticker.C:
			x._keepalive(ctx)
		}
	}
}

func (x *client) _keepalive(ctx context.Context) (err error) {
	for i := 0; i < len(x.cc); i++ {
		if err := x.cc[i].Ping(ctx); err != nil {
			return err
		}
	}
	return nil
}

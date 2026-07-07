package balancer

import (
	"context"
	"errors"
	"grpcx/resolver"
	"grpcx/status"
	"grpcx/ttrpc"
	"math/rand"
	"net/url"
	"sync/atomic"
	"time"
)

type RoundRobin struct {
	rts            []ttrpc.RoundTripper
	next           atomic.Uint32
	step           uint32
	dialContext    func(ctx context.Context) (ttrpc.RoundTripper, error)
	resolveContext func(ctx context.Context) ([]resolver.Address, error)
}

func (x *RoundRobin) Pick(_ context.Context, _ PickInfo) (ttrpc.RoundTripper, error) {
	if len(x.rts) == 0 {
		return nil, errors.New("no available RoundTripper")
	}
	idx := x.next.Add(x.step) % uint32(len(x.rts))
	return x.rts[idx], nil
}

func DialContext(ctx context.Context, target string, opts ...ttrpc.Option) (Picker, error) {
	var x RoundRobin = RoundRobin{
		step: 1,
		dialContext: func(ctx context.Context) (ttrpc.RoundTripper, error) {
			return ttrpc.DialContext(ctx, target, opts...)
		},
		resolveContext: func(ctx context.Context) ([]resolver.Address, error) {
			resolver := resolver.GetResolver("dns")
			adderss, err := resolver.Resolve(url.URL{Scheme: "dns", Host: target})
			if err != nil {
				return nil, err
			}
			return adderss, nil
		},
	}
	address, err := x.resolveContext(ctx)
	if err != nil {
		return nil, err
	}
	for range address {
		rt, err := x.dialContext(ctx)
		if err != nil {
			return nil, err
		}
		x.rts = append(x.rts, rt)
	}
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	next := uint32(rng.Intn(len(x.rts)))
	x.next.Store(next)
	return &x, nil
}

func (x *RoundRobin) wait(ctx context.Context) error {
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			address, err := x.resolveContext(ctx)
			if err != nil {
				continue
			}
			for i := len(x.rts); i <= len(address); i++ {
				rt, err := x.dialContext(ctx)
				if err != nil {
					continue
				}
				x.rts = append(x.rts, rt)
			}
		}
	}
}

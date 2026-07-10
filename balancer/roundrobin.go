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

type rrBuilder struct{}

func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...ttrpc.Option) (Picker, error) {
	childCtx, cancel := context.WithCancel(ctx)
	var x = RoundRobin{
		step: 1,
		dialContext: func(ctx context.Context) (ttrpc.RoundTripper, error) {
			return ttrpc.DialContext(ctx, endpoint, opts...)
		},
		resolveContext: func(ctx context.Context) ([]resolver.Address, error) {
			resolver := resolver.GetResolver("dns")
			address, err := resolver.Resolve(url.URL{Scheme: "dns", Host: endpoint})
			if err != nil {
				return nil, err
			}
			return address, nil
		},
		cancelFunc: cancel,
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
	go func() {
		if err := x.watch(childCtx, time.Minute); err != nil {
			return
		}
	}()
	return &x, nil
}

type RoundRobin struct {
	rts            []ttrpc.RoundTripper
	next           atomic.Uint32
	step           uint32
	dialContext    func(ctx context.Context) (ttrpc.RoundTripper, error)
	resolveContext func(ctx context.Context) ([]resolver.Address, error)
	cancelFunc     context.CancelFunc
}

func (x *RoundRobin) Pick(_ context.Context, _ PickInfo) (ttrpc.RoundTripper, error) {
	rts := x.rts
	if len(rts) == 0 {
		return nil, errors.New("no available RoundTripper")
	}
	idx := x.next.Add(x.step) % uint32(len(rts))
	return rts[idx], nil
}

func DialContext(ctx context.Context, endpoint string, opts ...ttrpc.Option) (Picker, error) {
	var b rrBuilder
	return b.Build(ctx, endpoint, opts...)
}

func (x *RoundRobin) watch(ctx context.Context, d time.Duration) error {
	ticker := time.NewTicker(d)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		case <-ticker.C:
			ips, err := x.resolveContext(ctx)
			if err != nil {
				continue
			}
			rts := x.rts
			for i := len(ips); i < len(rts); i++ {
				rts[i].Close()
			}
			for i := len(rts); i < len(ips); i++ {
				rt, err := x.dialContext(ctx)
				if err != nil {
					continue
				}
				rts = append(rts, rt)
			}
			if len(ips) < len(rts) {
				rts = rts[:len(ips)]
			}
			x.rts = rts
		}
	}
}

func (x *RoundRobin) Close() error {
	if x.cancelFunc != nil {
		x.cancelFunc()
	}
	rts := x.rts
	for i := range rts {
		rts[i].Close()
	}
	return nil
}

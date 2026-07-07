package balancer

import (
	"context"
	"errors"
	"grpcx/resolver"
	"grpcx/ttrpc"
	"math/rand"
	"net/url"
	"sync/atomic"
	"time"
)

type RoundRobin struct {
	rts  []ttrpc.RoundTripper
	next atomic.Uint32
	step uint32
}

func (x *RoundRobin) Pick(_ context.Context, _ PickInfo) (ttrpc.RoundTripper, error) {
	if len(x.rts) == 0 {
		return nil, errors.New("no available RoundTripper")
	}
	idx := x.next.Add(x.step) % uint32(len(x.rts))
	return x.rts[idx], nil
}

func DialContext(ctx context.Context, target string, opts ...ttrpc.Option) (Picker, error) {
	resolver := resolver.GetResolver("dns")
	adderss, err := resolver.Resolve(url.URL{Scheme: "dns", Host: target})
	if err != nil {
		return nil, err
	}
	var x RoundRobin = RoundRobin{
		step: 1,
	}
	for range adderss {
		rt, err := ttrpc.DialContext(ctx, target, opts...)
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

package balancer

import (
	"context"
	"errors"
	"grpcx/ttrpc"
	"math/rand"
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

func NewRoundRobinPicker(rts ...ttrpc.RoundTripper) Picker {
	var x RoundRobin
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	next := uint32(rng.Intn(len(rts)))
	x.rts = append(x.rts, rts...)
	x.next.Store(next)
	x.step = 1
	return &x
}

package balancer

import (
	"context"
	"errors"
	"grpcx/ttrpc"
	"sync/atomic"
)

type RoundRobin struct {
	rts []ttrpc.RoundTripper
	idx atomic.Uint32
}

func (x *RoundRobin) Pick(_ context.Context, _ PickInfo) (ttrpc.RoundTripper, error) {
	if len(x.rts) == 0 {
		return nil, errors.New("no available RoundTripper")
	}
	return x.rts[x.idx.Add(1)%uint32(len(x.rts))], nil
}

func NewRoundRobinPicker(rts ...ttrpc.RoundTripper) Picker {
	var x RoundRobin
	x.rts = append(x.rts, rts...)
	return &x
}

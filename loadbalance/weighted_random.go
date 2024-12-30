package loadbalance

import (
	"context"
	"grpcx/discovery"
	"math/rand/v2"
)

type randomPicker struct {
	instances []discovery.Instance
}

func newRandomPicker(instances []discovery.Instance) Picker {
	return &randomPicker{
		instances: instances,
	}
}

// Next implements the Picker interface.
func (rp *randomPicker) Next(ctx context.Context, request interface{}) (ins discovery.Instance) {
	idx := rand.IntN(len(rp.instances))
	return rp.instances[idx]
}

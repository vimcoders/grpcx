package balance

import (
	"context"
	"github/vimcoders/grpcx/discovery"
	"math/rand/v2"
)

type randomPicker struct {
	instances []discovery.Instance
}

func NewRandomPicker(instances []discovery.Instance) Picker {
	return &randomPicker{
		instances: instances,
	}
}

// Next implements the Picker interface.
func (x *randomPicker) Next(ctx context.Context, request interface{}) discovery.Instance {
	idx := rand.IntN(len(x.instances))
	return x.instances[idx]
}

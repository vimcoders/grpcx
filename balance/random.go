package balance

import (
	"context"
	"math/rand/v2"

	"github.com/vimcoders/grpcx/discovery"
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

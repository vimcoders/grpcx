package balancer

import (
	"context"
	"math/rand/v2"
	"ttrpc/driver"
)

type Rand struct {
	roundtrips []driver.RoundTripper
}

func (x *Rand) Pick(_ context.Context, _ driver.PickInfo) (driver.RoundTripper, error) {
	return x.roundtrips[rand.N(len(x.roundtrips))], nil
}

func NewRandPicker(rts ...driver.RoundTripper) driver.Picker {
	var x Rand
	x.roundtrips = append(x.roundtrips, rts...)
	return &x
}

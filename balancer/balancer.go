package balancer

import (
	"context"
	"io"

	"github.com/vimcoders/grpcx/transport"
)

// Picker is the interface for picking a round tripper from a list of round trippers.
type Picker interface {
	Pick(context.Context, PickInfo) (transport.RoundTripper, error)
	io.Closer
}

// PickInfo identifies the call that a backend is being picked for.
type PickInfo struct {
	FullMethodName string // the method being invoked, e.g. /api.EchoService/Echo
	Authority      int64
}

// Builder is the interface for building a balancer.
type Builder interface {
	Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error)
}

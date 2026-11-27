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

// PickInfo identifies the call that a backend is being picked for. Both fields
// are informational today: the client picks without naming the method, because
// backends come from the K8s Service or DNS and every endpoint is equally
// eligible, and Authority is not part of the frame header any more.
type PickInfo struct {
	FullMethodName string // the method being invoked, e.g. /api.EchoService/Echo
	Authority      int64  // unused; kept for callers that still set it
}

// Builder is the interface for building a balancer.
type Builder interface {
	Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error)
}

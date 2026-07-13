package balancer

import (
	"context"
	"io"

	"github.com/vimcoders/grpcx/roundtrip"
)

// Picker is the interface for picking a round tripper from a list of round trippers.
type Picker interface {
	Pick(context.Context, PickInfo) (roundtrip.RoundTripper, error)
	io.Closer
}

// PickInfo contains information about the request being made.
type PickInfo struct {
	FullMethodName string // 请求方法名
}

// Builder is the interface for building a balancer.
type Builder interface {
	Build(ctx context.Context, endpoint string, opts ...roundtrip.Option) (Picker, error)
}

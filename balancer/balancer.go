package balancer

import (
	"context"
	"grpcx/transport"
)

type Picker interface {
	Pick(context.Context, PickInfo) (transport.RoundTripper, error)
}

type PickInfo struct {
	FullMethodName string // 请求方法名
}

package balancer

import (
	"context"
	"grpcx/ttrpc"
)

type Picker interface {
	Pick(context.Context, PickInfo) (ttrpc.RoundTripper, error)
}

type PickInfo struct {
	FullMethodName string // 请求方法名
}

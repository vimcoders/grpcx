package driver

import "context"

type Picker interface {
	Pick(context.Context, PickInfo) (RoundTripper, error)
}

type PickInfo struct {
	FullMethodName string // 请求方法名
}

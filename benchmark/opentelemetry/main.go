package main

import (
	"context"
	"time"

	"go.opentelemetry.io/otel"
)

func main() {
	// 创建一个新的 trace
	tracer := otel.Tracer("example-tracer")
	ctx, span := tracer.Start(context.Background(), "root-span")
	// 暂停 100ms
	time.Sleep(100 * time.Millisecond)
	// 结束 span
	span.End()
	// 创建子span
	_, childSpan := tracer.Start(ctx, "child-span")
	// 暂停 50ms
	time.Sleep(50 * time.Millisecond)
	childSpan.End()
}

package grpcx

import "context"

const (
	grpc_authority = "Authority"
)

func WithAuthority(ctx context.Context, authority string) context.Context {
	return context.WithValue(ctx, grpc_authority, authority)
}

func GetAuthority(ctx context.Context) string {
	val, ok := ctx.Value(grpc_authority).(string)
	if !ok {
		return ""
	}
	return val
}

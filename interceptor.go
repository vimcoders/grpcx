package grpcx

import (
	"context"
	"grpcx/generated/api"
	"grpcx/ttrpc"

	"grpcx/status"

	"google.golang.org/grpc/codes"
)

type UnaryClientInterceptor func(context.Context, *api.Request, ttrpc.RoundTripper) (*api.Response, error)

func RetriesUnaryClientInterceptor(retries int32) UnaryClientInterceptor {
	return func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (*api.Response, error) {
		for i := retries; i >= 0; i-- {
			reply, err := rt.RoundTrip(ctx, r)
			if err != nil {
				return reply, err
			}
			if i == 0 {
				return reply, nil
			}
			code := codes.Code(reply.Code)
			switch code {
			case codes.OK:
				return reply, nil
			case codes.Unavailable:
				fallthrough
			case codes.DeadlineExceeded:
				fallthrough
			case codes.Internal:
				continue
			default:
				return reply, status.Error(code, reply.Message)
			}
		}
		return nil, status.OutOfRange.Err()
	}
}

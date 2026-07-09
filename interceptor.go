package grpcx

import (
	"context"
	"grpcx/generated/api"
	"grpcx/ttrpc"

	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
)

type UnaryClientInterceptor func(context.Context, *api.Request, ttrpc.RoundTripper) (*api.Response, error)

func RetriesUnaryClientInterceptor(retries int32) UnaryClientInterceptor {
	return func(ctx context.Context, r *api.Request, rt ttrpc.RoundTripper) (reply *api.Response, err error) {
		for range retries {
			reply, err = rt.RoundTrip(ctx, r)
			if err != nil {
				return reply, err
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
		return reply, err
	}
}

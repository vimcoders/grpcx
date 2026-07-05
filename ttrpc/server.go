package ttrpc

import (
	"context"
	"grpcx/generated/api"
	"grpcx/status"
	"net"

	"grpcx/encoding"
)

type ServerOption func(c *ServerOptions)

type ServerOptions struct {
	encoding.Codec
	RoundTripper
}

var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
}

func UnaryServerInterceptor(rt RoundTripper) ServerOption {
	return func(so *ServerOptions) {
		so.RoundTripper = rt
	}
}

func (x *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
	channel := newChannel(c)
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		default:
			streamID, payload, err := channel.Recv()
			if err != nil {
				return err
			}
			var request api.Request
			if err := x.Unmarshal(payload, &request); err != nil {
				return err
			}
			channel.putmbuf(payload)
			response, err := x.RoundTrip(ctx, &request)
			if err != nil {
				return err
			}
			b, err := x.Marshal(response)
			if err != nil {
				return err
			}
			if err := channel.Send(streamID, b); err != nil {
				return err
			}
		}
	}
}

package roundtrip

import (
	"context"
	"net"
	"path"
	"slices"
	"time"

	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/generated/api"

	"github.com/vimcoders/grpcx/encoding"

	"github.com/vimcoders/grpcx/metadata"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// Server is a ttrpc server that handles incoming requests and dispatches them to the appropriate service method.
type ServerOption func(c *ServerOptions)

// ServerOptions is a struct that holds the options for a ttrpc server.
type ServerOptions struct {
	encoding.Codec
	desc        *grpc.ServiceDesc
	imp         any
	interceptor grpc.UnaryServerInterceptor
}

// DefaultServerOptions is the default options for a ttrpc server.
var DefaultServerOptions = ServerOptions{
	Codec: encoding.GetCodec(encoding.Name),
	interceptor: func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (resp any, err error) {
		return handler(ctx, req)
	},
}

// NewServer creates a new ttrpc server with the given options.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return func(so *ServerOptions) {
		so.interceptor = interceptor
	}
}

// Codec sets the codec for the ttrpc server.
func RegisterService(sd *grpc.ServiceDesc, ss any) ServerOption {
	return func(so *ServerOptions) {
		so.desc = sd
		so.imp = ss
	}
}

// NewServer creates a new ttrpc server with the given options.
func (so *ServerOptions) RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error) {
	if req.Method == "" {
		return &api.Response{
			Code:    int32(codes.OK),
			Message: codes.OK.String(),
		}, nil
	}
	idx := slices.IndexFunc(so.desc.Methods, func(v grpc.MethodDesc) bool {
		return path.Join("/", so.desc.ServiceName, v.MethodName) == req.Method
	})
	if idx < 0 {
		return &api.Response{
			Code:    int32(codes.Unimplemented),
			Message: codes.Unimplemented.String(),
		}, nil
	}
	timeoutCtx, cancel := context.WithTimeout(ctx, time.Duration(req.Timeout)*time.Millisecond)
	defer cancel()
	reply, err := so.desc.Methods[idx].Handler(
		so.imp,
		metadata.WithMetadata(timeoutCtx, metadata.Pairs(req.Metadatas...)),
		func(in any) error {
			return so.Unmarshal(req.Payload, in)
		},
		so.interceptor)
	if err != nil {
		return &api.Response{
			Code:    int32(codes.Unavailable),
			Message: err.Error(),
		}, nil
	}
	response, err := so.Marshal(reply)
	if err != nil {
		return &api.Response{
			Code:    int32(codes.Unavailable),
			Message: err.Error(),
		}, nil
	}
	return &api.Response{
		Code:    int32(codes.OK),
		Message: codes.OK.String(),
		Payload: response,
	}, nil
}

// Handle handles incoming requests on the given net.Conn. It reads requests from the connection, dispatches them to the appropriate service method, and writes responses back to the connection.
func (so *ServerOptions) Handle(ctx context.Context, c net.Conn) (err error) {
	defer c.Close()
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
			if err := so.Unmarshal(payload, &request); err != nil {
				return err
			}
			channel.putmbuf(payload)
			response, err := so.RoundTrip(ctx, &request)
			if err != nil {
				return err
			}
			b, err := so.Marshal(response)
			if err != nil {
				return err
			}
			if err := channel.Send(streamID, b); err != nil {
				return err
			}
		}
	}
}

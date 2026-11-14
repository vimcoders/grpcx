package transport

import (
	"context"
	"io"
	"math"
	"time"

	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

// RoundTripper is the client-side transport: it dials a connection, dispatches
// unary calls over it and keeps it alive. A RoundTripper is safe for concurrent
// use by multiple goroutines.
type RoundTripper interface {
	// RoundTrip sends an already framed request and waits for the response.
	RoundTrip(ctx context.Context, req *api.Request) (*api.Response, error)
	// Ping sends an empty request to keep the connection from hitting the read
	// deadline.
	Ping(ctx context.Context) error
	grpc.ClientConnInterface
	io.Closer
}

// Stream is the message-level half of a stream: it sends and receives one
// message at a time. grpcx carries unary calls only, so each stream carries
// exactly one request and one response.
type Stream interface {
	Send(any, ...CallOption) error
	Recv(any) error
}

// ClientStream is the client-side stream: the message level Stream plus the
// grpc.ClientStream surface the generated stubs and interceptors are typed
// against.
type ClientStream interface {
	Stream
	grpc.ClientStream
}

// ServerStream is the server-side stream: the message level Stream plus the
// grpc.ServerStream surface the generated handlers and interceptors are typed
// against.
type ServerStream interface {
	Stream
	grpc.ServerStream
}

const (
	// messageHeaderLength is the fixed frame header: a 4-byte payload length.
	// The stream id travels inside the protobuf header frame, not the frame header.
	messageHeaderLength = 4
	// messageLengthMax caps a single frame payload, aligned with the standard
	// gRPC default.
	messageLengthMax = 4 * 1024 * 1024
	// defaultFlushInterval is the timer resolution Go's runtime timer wheel
	// settles on, so it is the shortest useful flush delay.
	defaultFlushInterval = time.Millisecond
	// defaultBufSize is one batching block. bufsize is counted in blocks.
	defaultBufSize = 1024
	// defaultMaxBufSize bounds the send buffer at 16 blocks, matching the
	// default TCP send buffer on Linux.
	defaultMaxBufSize = 16 * defaultBufSize
	// defaultMaxStreams is the default maximum number of concurrent streams
	// per connection.
	defaultMaxStreams = math.MaxUint16
	// defaultTimeout is the default timeout of one request.
	defaultTimeout = 3 * time.Second
	// defaultReadTimeout bounds how long the reader waits for the next frame
	// before treating the connection as dead. It must stay well above the
	// keepalive period, or idle connections would be torn down between pings.
	defaultReadTimeout = 10 * defaultTimeout
	// defaultWriteTimeout bounds a single flush.
	defaultWriteTimeout = defaultTimeout
	// defaultKeepalive is the idle period, in units of defaultTimeout, at which
	// the round robin balancer pings a connection.
	defaultKeepalive = 3
)

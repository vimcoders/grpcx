package transport

import (
	"context"
	"io"
	"math"
	"net"
	"time"

	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

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

// Sender writes frames onto one connection: every argument becomes a single
// length-prefixed frame, so a caller sends the header and the payload as two
// frames.
type Sender interface {
	Send(...[]byte) error
}

// Receiver reads one frame at a time with the length header already stripped.
// The buffer is owned by the caller; package callers return it to the pool with
// putmbuf.
type Receiver interface {
	Recv() ([]byte, error)
}

// SendCloser is a Sender that also closes the connection, flushing whatever
// batching still holds before it does.
type SendCloser interface {
	Sender
	io.Closer
}

// RecvCloser is a Receiver that also closes the connection.
type RecvCloser interface {
	Receiver
	io.Closer
}

// Handler serves one accepted connection. It owns the framing decisions for
// every call it answers: the built-in handler reads one request header frame
// plus its payload frame and dispatches it to a registered unary method, while a
// replacement — a proxy forwarding frames verbatim, or another protocol layered
// on the same framing — takes over that loop instead.
//
// Handle is called once per accepted connection and returns when the connection
// breaks or ctx is canceled; returning an error closes the connection.
type Handler interface {
	Handle(ctx context.Context, c net.Conn) error
}

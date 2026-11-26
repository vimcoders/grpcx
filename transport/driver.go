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
	// readBufSize is the per-connection read-ahead; the channel sizes its
	// bufio.Reader to min(messageLengthMax, readBufSize). It is kept at a
	// quarter of messageLengthMax, because the reader only has to cover the
	// 4-byte frame header and the payloads that arrive in bursts: a payload at
	// least this large is read directly once the buffer drains, while a
	// smaller one comes out of the buffer. messageLengthMax itself cost 4MB
	// of heap per connection.
	readBufSize = 1024 * 1024
	// defaultFlushInterval is the timer resolution Go's runtime timer wheel
	// settles on, so it is the shortest useful flush delay, and it is also the
	// only one a batched connection runs at: Options carries no interval field.
	defaultFlushInterval = time.Millisecond
	// defaultBufSize is the size of one batching block. WithBufSize multiplies
	// its argument by it, so Options.bufsize — the field channel.Send compares
	// the pending frames against — is counted in bytes. The server option keeps
	// its argument as a block count instead, and Handle does the conversion.
	defaultBufSize = 1024
	// defaultMaxBufSize bounds the send buffer at 16 blocks, matching the
	// default TCP send buffer on Linux.
	defaultMaxBufSize = 16 * defaultBufSize
	// defaultMaxStreams is the default maximum number of concurrent streams
	// per connection.
	defaultMaxStreams = math.MaxUint16
	// defaultTimeout is the default per-request timeout stored in Options, and the
	// value defaultWriteTimeout derives from. Client streams no longer run under
	// it: they are cut at defaultReadTimeout, and the server clamps a handler to
	// WithServerTimeout.
	defaultTimeout = 3 * time.Second
	// defaultReadTimeout bounds how long the reader waits for the next frame
	// before treating the connection as dead. It must stay well above the
	// keepalive period, or idle connections would be torn down between pings.
	defaultReadTimeout = 3 * defaultKeepalive
	// defaultWriteTimeout bounds a single write: Send arms the write deadline on
	// every call, and the flush timer arms one of its own before it writes.
	defaultWriteTimeout = defaultTimeout
	// defaultKeepalive is the idle period at which the round robin balancer pings
	// a connection. Ten seconds is the floor gRPC puts on a client keepalive, and
	// the read timeout is three of these intervals, so an idle connection gets two
	// pings before the reader gives up on it.
	defaultKeepalive = 10 * time.Second
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

// Recver reads one frame at a time with the length header already stripped.
// The buffer is owned by the caller; package callers return it to the pool with
// putmbuf.
type Recver interface {
	Recv() ([]byte, error)
}

// SendCloser is a Sender that also closes the connection. Frames still held in
// the send buffer are dropped at that point rather than written out first.
type SendCloser interface {
	Sender
	io.Closer
}

// RecvCloser is a Recver that also closes the connection.
type RecvCloser interface {
	Recver
	io.Closer
}

// Handler serves one accepted connection. Handle is called once per accepted
// connection, owns every frame read from and written to it, and returns when
// the connection breaks or ctx is canceled; returning an error closes the
// connection. ServerOptions implements it, which is what a Server embeds to
// expose the per-connection entry point.
type Handler interface {
	Handle(ctx context.Context, c net.Conn) error
}

// Channel is one connection seen as both ends of the framing: a handler reads
// the request frame with Recv and writes the reply frame with Send, on the same
// connection, so the two are passed together rather than as separate halves.
type Channel interface {
	Sender
	Recver
}

// ServiceRegistrar is the registration half of the gRPC server interface.
// ServerOptions implements it, so a Server can embed it and keep
// RegisterService a promoted method without owning a field for it.
type ServiceRegistrar = grpc.ServiceRegistrar

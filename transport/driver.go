package transport

import (
	"context"
	"io"
	"math"
	"net"
	"time"

	"google.golang.org/grpc"
)

const (
	// messageHeaderLength is the fixed frame header: a 4-byte payload length.
	// The stream id travels inside the protobuf header frame, not the frame
	// header.
	messageHeaderLength = 4
	// messageLengthMax caps a single frame payload, aligned with the standard
	// gRPC default.
	messageLengthMax = 4 * 1024 * 1024
	// readBufSize is the per-connection read-ahead; the channel sizes its
	// bufio.Reader to min(messageLengthMax, readBufSize). A quarter of
	// messageLengthMax is enough: the reader only has to cover the 4-byte frame
	// header and payloads that arrive in bursts. A payload at least this large
	// is read directly once the buffer drains. messageLengthMax itself cost
	// 4MB of heap per connection.
	readBufSize = 32 * 1024
	// defaultFlushInterval is the timer resolution Go's runtime timer wheel
	// settles on, so it is the shortest useful flush delay, and the only one a
	// batched connection runs at: Options carries no interval field.
	defaultFlushInterval = time.Millisecond
	// defaultBufSize is the size of one batching block. WithBufSize takes bytes
	// directly, so this constant only sets the defaultMaxBufSize ceiling:
	// sixteen blocks.
	defaultBufSize = 1024
	// defaultMaxBufSize bounds the send buffer at 16 blocks, matching the
	// default TCP send buffer on Linux.
	defaultMaxBufSize = 16 * defaultBufSize
	// defaultMaxStreams is the default maximum number of concurrent streams
	// per connection.
	defaultMaxStreams = math.MaxUint16
	// defaultTimeout is the default per-request timeout stored in Options, the
	// value defaultWriteTimeout derives from, and the starting point of a ping's
	// budget. Client streams do not run under it: they are cut at
	// defaultReadTimeout, and the server clamps a handler to WithServerTimeout.
	// Ping caps it at defaultKeepalive.
	defaultTimeout = time.Second
	// defaultReadTimeout bounds how long the reader waits for the next frame
	// before treating the connection as dead. It must stay well above the
	// keepalive period, or idle connections are torn down between pings.
	defaultReadTimeout = 3 * defaultKeepalive
	// defaultWriteTimeout bounds a single write: Send arms the write deadline on
	// every call, and the flush timer writes under the deadline the last Send
	// armed.
	defaultWriteTimeout = defaultTimeout
	// defaultKeepalive is the idle period at which the round robin balancer pings
	// a connection. It is three defaultTimeouts rather than gRPC's ten-second
	// client keepalive floor: grpcx carries the ping itself, so no peer applies
	// that policy, and a ping has to fail before the next one is due. The read
	// timeout is three of these intervals, so an idle connection gets two pings
	// before the reader gives up on it.
	defaultKeepalive = 3 * defaultTimeout
)

// RoundTripper is the client-side transport: it dials a connection, dispatches
// unary calls over it and keeps it alive. A RoundTripper is safe for concurrent
// use by multiple goroutines.
type RoundTripper interface {
	// RoundTrip sends p as one payload frame on a fresh stream and returns the
	// reply payload as a Buffer the caller owns and frees. The bytes go on the
	// wire as given: p is a message body the caller already has, not an object
	// to encode.
	//
	// With batching off (the default) p is written before RoundTrip returns.
	// Once WithBufSize is set, the frame can sit in the pending batch for up to
	// one flush interval: the caller must neither change the bytes nor free the
	// buffer holding them until the flush.
	RoundTrip(context.Context, []byte, ...CallOption) (Buffer, error)
	// Ping sends an empty request to keep the connection from hitting the read
	// deadline.
	Ping(context.Context) error
	grpc.ClientConnInterface
	io.Closer
}

// Sender writes frames onto one connection: every argument becomes a single
// length-prefixed frame, so a caller sends the header and the payload as two
// frames. The slices are borrowed, not owned: a batched channel keeps them by
// reference until the flush, so the caller must not change or free them before
// that write has happened.
type Sender interface {
	Send(...[]byte) error
}

// Recver reads one frame at a time with the length header already stripped.
// The buffer is owned by the caller, and its Free returns it to the pool; a
// zero-length frame comes back as an empty Buffer rather than nil.
type Recver interface {
	Recv() (Buffer, error)
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
// the request frame with Recv and writes the reply frame with Send on the same
// connection, so the two are passed together.
type Channel interface {
	Sender
	Recver
}

// ServiceRegistrar is the registration half of the gRPC server interface.
// ServerOptions implements it, so a Server can embed it and keep
// RegisterService a promoted method without owning a field for it.
type ServiceRegistrar = grpc.ServiceRegistrar

// Buffer is one frame payload and the right to free it: ReadOnlyData is the
// bytes as they arrived, Free returns the storage to the pool.
//
// Free is not idempotent — the value carries no reference count — so a Buffer
// has exactly one owner at a time and is freed exactly once. RoundTrip hands
// the reply over to the caller under that rule; the server side frees a
// buffer as soon as the handler is done with it.
type Buffer interface {
	Free()
	ReadOnlyData() []byte
}

// Codec turns one message into a Buffer and back.
//
// Marshal returns a buffer the caller owns and frees: it can come from the
// frame pool, in which case Free returns it, or from an encoding that allocates
// its own bytes, in which case Free is a no-op. Unmarshal only reads the
// buffer; ownership stays with the caller.
//
// Two implementations exist. defaultCodec special-cases proto.Message into a
// pool buffer and delegates everything else to the configured encoding.Codec;
// codec delegates every value, proto included. The server picks between them
// according to its batching threshold; the client always uses defaultCodec.
type Codec interface {
	Marshal(any) (Buffer, error)
	Unmarshal(Buffer, any) error
}

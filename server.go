package grpcx

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

// ServerOption configures a Server. It is an alias of transport.ServerOption,
// so the two packages accept the same values.
type ServerOption = transport.ServerOption

// UnaryServerInterceptor sets the interceptor that wraps every unary call.
func UnaryServerInterceptor(interceptor grpc.UnaryServerInterceptor) ServerOption {
	return transport.UnaryServerInterceptor(interceptor)
}

// WithServerCodec sets the codec used to unmarshal requests and marshal
// replies on the server side.
func WithServerCodec(codec encoding.Codec) ServerOption {
	return transport.WithServerCodec(codec)
}

// WithServerReadTimeout sets how long the server waits for the next frame
// before giving up on the connection. It reaches every accepted connection
// through transport.WithReadTimeout.
func WithServerReadTimeout(d time.Duration) ServerOption {
	return transport.WithServerReadTimeout(d)
}

// WithServerWriteTimeout sets how long the server waits for a write to flush
// before failing the call. It reaches every accepted connection through
// transport.WithWriteTimeout.
func WithServerWriteTimeout(d time.Duration) ServerOption {
	return transport.WithServerWriteTimeout(d)
}

// WithServerBufSize caps the send buffer in blocks of defaultBufSize,
// clamped to defaultMaxBufSize/defaultBufSize. A value of zero disables
// batching and makes every Send synchronous.
func WithServerBufSize(n int) ServerOption {
	return transport.WithServerBufSize(n)
}

// Handler serves one accepted connection at the transport level: it reads and
// writes frames directly and returns when the connection breaks or ctx is
// canceled.
type Handler = transport.Handler

// ServiceRegistrar is the registration half of the gRPC server interface. It is
// embedded so RegisterService is promoted onto Server as a method.
type ServiceRegistrar = transport.ServiceRegistrar

// Server accepts connections and serves each one with the built-in unary
// dispatcher over the registered service.
type Server struct {
	Handler
	ServiceRegistrar
}

// NewServer returns a server configured by opts. Options not passed here are
// never applied later, so the interceptor has to be installed at construction
// time; the service itself is registered through RegisterService.
func NewServer(opts ...ServerOption) *Server {
	defaultOptions := transport.DefaultServerOptions
	for _, v := range opts {
		v(&defaultOptions)
	}
	return &Server{
		Handler:          &defaultOptions,
		ServiceRegistrar: &defaultOptions,
	}
}

// Close reports nil and does nothing else: it does not stop the serve loop, does
// not close the listener and does not touch any connection. Returning without
// waiting is deliberate, because handlers can still be in flight when it
// returns; the wait happens at the end of ListenAndServe instead. Every call
// returns nil, including on a Server that never served.
func (s *Server) Close() error {
	return nil
}

// ListenAndServe accepts connections on addr and serves each one in its own
// goroutine until the listener fails or ctx is canceled. An AfterFunc closes the
// listener when ctx is done, which is what wakes the loop out of Accept; the
// closed listener then surfaces as the returned error, so a canceled context
// comes back as net.ErrClosed rather than nil. The ctx also reaches the
// handlers, so they see the same cancellation.
//
// The handlers started here are waited for before the return: the deferred wait
// runs after the accept loop has exited, so by the time ListenAndServe returns
// every accepted connection has been served to its end. That wait is also why a
// handler parked on an idle connection delays the return until its read
// deadline fires.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	defer s.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		wg.Go(func() {
			if err := s.Handle(ctx, conn); err != nil {
				slog.Debug("listenAndServe: exited", "error", err)
			}
		})
	}
}

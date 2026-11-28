package grpcx

import (
	"context"
	"errors"
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

// WithServerCodec stores the codec in the transport options and rebuilds the
// server Codec from it, keeping the batching choice intact. It can be passed
// before or after WithServerBufSize: the later of the two re-derives the Codec.
// The request header is still decoded with the built-in codec of package
// encoding whatever is configured here.
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

// WithServerBufSize applies the byte threshold and rebuilds the server Codec:
// an unpooled codec while the threshold is positive, the pool-backed
// defaultCodec when it is non-positive, which also turns batching off.
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
	defaultOptions := transport.DefaultServerOptions()
	for _, v := range opts {
		v(&defaultOptions)
	}
	return &Server{
		Handler:          &defaultOptions,
		ServiceRegistrar: &defaultOptions,
	}
}

// ListenAndServe accepts connections on addr and serves each one in its own
// goroutine until the listener fails or ctx is canceled.
//
// An AfterFunc closes the listener when ctx is done, which wakes the loop out of
// Accept; the net.ErrClosed that Accept then reports is folded into nil, so a
// canceled context is a clean stop. Any other accept error is returned as
// itself, and the same ctx reaches the handlers.
//
// The handlers are waited for before the return: the deferred wait runs after
// the accept loop has exited, so every accepted connection has been served to
// its end. A handler parked on an idle connection is woken by the same
// AfterFunc — Handle closes its channel on that cancel — so the wait is as long
// as the handler takes to notice, not as long as its read deadline.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	stop := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stop()
	var wg sync.WaitGroup
	defer wg.Wait()
	for {
		conn, err := listener.Accept()
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return nil
			}
			return err
		}
		wg.Go(func() {
			if err := s.Handle(ctx, conn); err != nil {
				slog.Debug("listenAndServe: exited", "error", err)
			}
		})
	}
}

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
// before giving up on the connection.
func WithServerReadTimeout(d time.Duration) ServerOption {
	return transport.WithServerReadTimeout(d)
}

// WithServerWriteTimeout sets how long the server waits for a write to flush
// before failing the call.
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
	wg       sync.WaitGroup
	listener net.Listener
	closed   context.CancelFunc
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

// Close stops accepting, closes the listener and waits for the connection
// handlers to return.
func (s *Server) Close() error {
	if s.closed != nil {
		s.closed()
	}
	if s.listener != nil {
		s.listener.Close()
	}
	s.wg.Wait()
	return nil
}

// ListenAndServe accepts connections on addr and serves each one in its own
// goroutine until the listener fails or ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	s.listener = listener
	cancelCtx, closed := context.WithCancel(ctx)
	s.closed = closed
	defer s.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		s.wg.Go(func() {
			if err := s.Handle(cancelCtx, conn); err != nil {
				slog.Error("ListenAndServe", "exited", err.Error())
			}
		})
	}
}

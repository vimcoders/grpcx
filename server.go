package grpcx

import (
	"context"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/status"
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
	wg sync.WaitGroup

	// mu guards the fields below: ListenAndServe installs the listener and the
	// cancel function from the serving goroutine while Close reads them from the
	// caller's, and both walk the set of open connections.
	mu       sync.Mutex
	listener net.Listener
	closed   context.CancelFunc
	conns    map[net.Conn]struct{}
	closing  bool
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

// Close stops accepting, closes the listener, closes every connection still
// being served and waits for the handlers to return. Closing the connections is
// what makes shutdown prompt: a handler parked in a read would otherwise sit
// there until its read timeout.
func (s *Server) Close() error {
	s.mu.Lock()
	closed, listener := s.closed, s.listener
	s.closing = true
	conns := make([]net.Conn, 0, len(s.conns))
	for conn := range s.conns {
		conns = append(conns, conn)
	}
	s.mu.Unlock()

	if closed != nil {
		closed()
	}
	if listener != nil {
		_ = listener.Close()
	}
	for _, conn := range conns {
		_ = conn.Close()
	}
	s.wg.Wait()
	return nil
}

// trackConn registers an accepted connection so Close can close it. It reports
// false once Close has run, in which case the caller must not serve the
// connection.
func (s *Server) trackConn(conn net.Conn) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	if s.conns == nil {
		s.conns = make(map[net.Conn]struct{})
	}
	s.conns[conn] = struct{}{}
	return true
}

func (s *Server) untrackConn(conn net.Conn) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.conns, conn)
}

// ListenAndServe accepts connections on addr and serves each one in its own
// goroutine until the listener fails or ctx is canceled.
func (s *Server) ListenAndServe(ctx context.Context, addr string) error {
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	cancelCtx, closed := context.WithCancel(ctx)
	s.mu.Lock()
	s.listener, s.closed = listener, closed
	s.mu.Unlock()
	defer s.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		if !s.trackConn(conn) {
			_ = conn.Close()
			return status.Canceled.Err()
		}
		s.wg.Go(func() {
			defer s.untrackConn(conn)
			if err := s.Handle(cancelCtx, conn); err != nil {
				slog.Error("ListenAndServe", "exited", err.Error())
			}
		})
	}
}

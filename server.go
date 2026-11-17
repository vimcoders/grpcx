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

type Server struct {
	transport.ServerOptions
	wg       sync.WaitGroup
	listener net.Listener
	closed   context.CancelFunc
}

func NewServer(opts ...ServerOption) *Server {
	opt := transport.DefaultServerOptions
	for _, v := range opts {
		v(&opt)
	}
	return &Server{
		ServerOptions: opt,
	}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (s *Server) RegisterService(sd *grpc.ServiceDesc, ss any) {
	transport.RegisterService(sd, ss)(&s.ServerOptions)
}

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

func (s *Server) ListenAndServe(ctx context.Context, addr string, opt ...transport.ServerOption) error {
	for i := range opt {
		opt[i](&s.ServerOptions)
	}
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

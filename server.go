package grpcx

import (
	"context"
	"grpcx/ttrpc"
	"net"
	"sync"

	"google.golang.org/grpc"
)

type Server struct {
	ttrpc.ServerOptions
	wg       sync.WaitGroup
	listener net.Listener
	closed   context.CancelFunc
}

func NewServer(opt ...ttrpc.ServerOption) *Server {
	opts := ttrpc.DefaultServerOptions
	for i := range opt {
		opt[i](&opts)
	}
	return &Server{
		ServerOptions: opts,
	}
}

// RegisterService registers a service and its implementation to the gRPC
// server. It is called from the IDL generated code. This must be called before
// invoking Serve. If ss is non-nil (for legacy code), its type is checked to
// ensure it implements sd.HandlerType.
func (s *Server) RegisterService(sd *grpc.ServiceDesc, ss any) {
	ttrpc.RegisterService(sd, ss)(&s.ServerOptions)
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

func (s *Server) ListenAndServe(ctx context.Context, addr string, opt ...ttrpc.ServerOption) error {
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
			_ = s.Handle(cancelCtx, conn)
		})
	}
}

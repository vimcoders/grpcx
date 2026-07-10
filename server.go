package grpcx

import (
	"context"
	"grpcx/ttrpc"
	"log/slog"
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
func (x *Server) RegisterService(sd *grpc.ServiceDesc, ss any) {
	ttrpc.RegisterService(sd, ss)(&x.ServerOptions)
}

func (x *Server) Close() error {
	if x.closed != nil {
		x.closed()
	}
	if x.listener != nil {
		x.listener.Close()
	}
	x.wg.Wait()
	return nil
}

func (x *Server) ListenAndServe(ctx context.Context, addr string, opt ...ttrpc.ServerOption) error {
	for i := range opt {
		opt[i](&x.ServerOptions)
	}
	listener, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	x.listener = listener
	cancelCtx, closed := context.WithCancel(ctx)
	x.closed = closed
	defer x.Close()
	for {
		conn, err := listener.Accept()
		if err != nil {
			return err
		}
		x.wg.Go(func() {
			if err := x.Handle(cancelCtx, conn); err != nil {
				slog.Error("grpcx disconnected", "Handle", err.Error())
			}
		})
	}
}

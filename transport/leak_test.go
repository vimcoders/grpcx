package transport

import (
	"context"
	"net"
	"runtime"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

// waitGoroutines waits until the goroutine count drops to want, or the budget
// runs out, and returns the last count it saw.
func waitGoroutines(want int, budget time.Duration) int {
	deadline := time.Now().Add(budget)
	got := runtime.NumGoroutine()
	for got > want && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
		got = runtime.NumGoroutine()
	}
	return got
}

func goroutineStacks() string {
	buf := make([]byte, 1<<20)
	n := runtime.Stack(buf, true)
	return string(buf[:n])
}

// connHandler adapts a function to the Handler interface.
type connHandler func(ctx context.Context, c net.Conn) error

func (f connHandler) Handle(ctx context.Context, c net.Conn) error { return f(ctx, c) }

// echoDesc is a minimal unary service descriptor: one Echo method that returns
// the message it received.
func echoDesc() *grpc.ServiceDesc {
	return &grpc.ServiceDesc{
		ServiceName: "echo",
		Methods: []grpc.MethodDesc{{
			MethodName: "Echo",
			Handler: func(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
				in := new(api.EchoRequest)
				if err := dec(in); err != nil {
					return nil, err
				}
				echo := func(ctx context.Context, req any) (any, error) {
					return &api.EchoResponse{Message: req.(*api.EchoRequest).GetMessage()}, nil
				}
				if interceptor == nil {
					return echo(ctx, in)
				}
				return interceptor(ctx, in, &grpc.UnaryServerInfo{Server: srv, FullMethod: "/echo/Echo"}, echo)
			},
		}},
	}
}

// TestConnectionGoroutinesExitAfterClose serves several connections through
// the built-in handler, closes each one, and requires the goroutine count to
// come back to the baseline. A connection goroutine that outlives its
// connection shows up here as an extra count plus its stack.
func TestConnectionGoroutinesExitAfterClose(t *testing.T) {
	// Let goroutines left by earlier tests finish before taking the baseline.
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	desc := echoDesc()
	const rounds = 5
	for i := 0; i < rounds; i++ {
		server, client := net.Pipe()
		so := DefaultServerOptions
		RegisterService(desc, struct{}{})(&so)
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() {
			defer close(done)
			_ = so.Handle(ctx, server)
		}()

		ch := newChannel(client, WithBufSize(0))
		h, err := encoding.Marshal(&api.CallOptions{FullMethodName: "/echo/Echo", StreamID: uint32(i + 1)})
		if err != nil {
			t.Fatal(err)
		}
		b, err := encoding.Marshal(&api.EchoRequest{Message: "in"})
		if err != nil {
			t.Fatal(err)
		}
		if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
			t.Fatal(err)
		}
		if err := ch.Send(h, b); err != nil {
			t.Fatalf("round %d: send: %v", i, err)
		}
		if _, err := ch.Recv(); err != nil { // response header
			t.Fatalf("round %d: recv header: %v", i, err)
		}
		if _, err := ch.Recv(); err != nil { // response payload
			t.Fatalf("round %d: recv payload: %v", i, err)
		}

		cancel()
		client.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("round %d: handler goroutine still running after close", i)
		}
		server.Close()
	}

	if got := waitGoroutines(base, 2*time.Second); got != base {
		t.Fatalf("goroutines = %d, want %d after %d closed connections\n%s",
			got, base, rounds, goroutineStacks())
	}
}

// TestReplacedHandlerGoroutineExits checks the same guarantee for a Handler
// installed through WithServerHandler: it returns and its goroutine goes away.
func TestReplacedHandlerGoroutineExits(t *testing.T) {
	time.Sleep(50 * time.Millisecond)
	base := runtime.NumGoroutine()

	server, client := net.Pipe()
	so := DefaultServerOptions
	WithServerHandler(connHandler(func(ctx context.Context, c net.Conn) error {
		ch := newChannel(c, WithBufSize(0))
		defer ch.Close()
		// Drain the request, answer once, then return.
		if _, err := ch.Recv(); err != nil {
			return err
		}
		if _, err := ch.Recv(); err != nil {
			return err
		}
		return ch.Send([]byte("pong"))
	}))(&so)

	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = so.Handle(context.Background(), server)
	}()

	ch := newChannel(client, WithBufSize(0))
	h, err := encoding.Marshal(&api.CallOptions{FullMethodName: "/echo/Echo", StreamID: 1})
	if err != nil {
		t.Fatal(err)
	}
	b, err := encoding.Marshal(&api.EchoRequest{Message: "in"})
	if err != nil {
		t.Fatal(err)
	}
	if err := client.SetReadDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	if err := ch.Send(h, b); err != nil {
		t.Fatal(err)
	}
	if _, err := ch.Recv(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("custom handler goroutine still running after it returned")
	}
	client.Close()
	server.Close()

	if got := waitGoroutines(base, 2*time.Second); got != base {
		t.Fatalf("goroutines = %d, want %d after the custom handler returned\n%s",
			got, base, goroutineStacks())
	}
}

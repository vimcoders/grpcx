package transport

import (
	"context"
	"io"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"google.golang.org/grpc"
)

// newTestConns returns the two ends of a loopback TCP connection. TCP rather
// than net.Pipe: net.Pipe is unbuffered, so a writer blocks until the reader
// drains it, which would deadlock every batching test.
func newTestConns(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	accepted := make(chan net.Conn, 1)
	go func() {
		c, acceptErr := ln.Accept()
		if acceptErr != nil {
			accepted <- nil
			return
		}
		accepted <- c
	}()

	c, err := net.Dial("tcp", ln.Addr().String())
	if err != nil {
		t.Fatalf("dial: %v", err)
	}

	var peer net.Conn
	select {
	case peer = <-accepted:
	case <-time.After(5 * time.Second):
		_ = c.Close()
		t.Fatalf("accept timed out")
	}
	if peer == nil {
		_ = c.Close()
		t.Fatalf("accept failed")
	}

	t.Cleanup(func() {
		_ = c.Close()
		_ = peer.Close()
	})
	return c, peer
}

// frameSink records every frame a Sender writes, so a test can assert on the
// exact bytes and their order.
type frameSink struct {
	mu     sync.Mutex
	frames [][]byte
}

func (f *frameSink) Send(p ...[]byte) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range p {
		f.frames = append(f.frames, append([]byte(nil), b...))
	}
	return nil
}

// Frames returns a copy of the recorded frames.
func (f *frameSink) Frames() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([][]byte, len(f.frames))
	for i, b := range f.frames {
		out[i] = append([]byte(nil), b...)
	}
	return out
}

// frameSource replays pre-recorded frames as a Recver. Frames are handed out
// through the buffer pool, because callers put them back with putmbuf and the
// pool only accepts buffers it could have handed out itself.
type frameSource struct {
	frames [][]byte
	err    error
}

func (f *frameSource) Recv() ([]byte, error) {
	if len(f.frames) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	b := f.frames[0]
	f.frames = f.frames[1:]
	p := getmbuf(len(b))
	copy(p, b)
	return p, nil
}

// newFrameSource builds a Recver that hands out the given frames in order.
func newFrameSource(frames ...[]byte) *frameSource {
	return &frameSource{frames: frames}
}

// echoMethodHandler is the grpc.MethodDesc handler shape for Echo, written out
// by hand so the tests do not depend on the generated helper, which is
// unexported.
func echoMethodHandler(srv any, ctx context.Context, dec func(any) error, interceptor grpc.UnaryServerInterceptor) (any, error) {
	in := new(api.EchoRequest)
	if err := dec(in); err != nil {
		return nil, err
	}
	if interceptor == nil {
		return srv.(api.EchoServiceServer).Echo(ctx, in)
	}
	info := &grpc.UnaryServerInfo{Server: srv, FullMethod: api.EchoService_Echo_FullMethodName}
	return interceptor(ctx, in, info, func(ctx context.Context, req any) (any, error) {
		return srv.(api.EchoServiceServer).Echo(ctx, req.(*api.EchoRequest))
	})
}

// testEcho is a controllable EchoServiceServer: it echoes the message, counts
// calls, records metadata and can be gated, failed or delayed on demand.
type testEcho struct {
	api.UnimplementedEchoServiceServer

	mu     sync.Mutex
	calls  int
	values map[string]string

	// gate, when non-nil, holds the handler until it is closed or the call
	// context ends.
	gate chan struct{}
	// err, when non-nil, is returned instead of a reply.
	err error
}

func (e *testEcho) Echo(ctx context.Context, in *api.EchoRequest) (*api.EchoResponse, error) {
	e.mu.Lock()
	e.calls++
	if md, ok := metadata.GetMetadata(ctx); ok {
		if e.values == nil {
			e.values = make(map[string]string, len(md))
		}
		for k, v := range md {
			e.values[k] = v
		}
	}
	e.mu.Unlock()

	e.mu.Lock()
	gate, err := e.gate, e.err
	e.mu.Unlock()

	if gate != nil {
		select {
		case <-gate:
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
	if err != nil {
		return nil, err
	}
	return &api.EchoResponse{Message: in.GetMessage()}, nil
}

func (e *testEcho) metadataValue(key string) string {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.values[key]
}

func (e *testEcho) callCount() int {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.calls
}

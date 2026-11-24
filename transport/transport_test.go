package transport

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/metadata"
	"github.com/vimcoders/grpcx/status"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// startServer serves the given options on a loopback listener and returns the
// address. Every accepted connection is served by the same ServerOptions, and
// the cleanup closes both the listener and the live connections before waiting:
// a handler parked in a read otherwise keeps the test process alive until its
// read timeout.
func startServer(t *testing.T, opts ...ServerOption) string {
	t.Helper()
	so := DefaultServerOptions
	for _, o := range opts {
		o(&so)
	}
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())

	var (
		mu    sync.Mutex
		conns []net.Conn
		wg    sync.WaitGroup
	)
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			conns = append(conns, conn)
			mu.Unlock()
			wg.Go(func() {
				defer func() { _ = conn.Close() }()
				_ = so.Handle(ctx, conn)
			})
		}
	}()
	t.Cleanup(func() {
		cancel()
		_ = ln.Close()
		mu.Lock()
		for _, c := range conns {
			_ = c.Close()
		}
		mu.Unlock()
		wg.Wait()
	})
	return ln.Addr().String()
}

// dialTransport connects to addr and closes the transport when the test ends.
func dialTransport(t *testing.T, addr string, opts ...Option) RoundTripper {
	t.Helper()
	rt, err := DialContext(context.Background(), addr, opts...)
	if err != nil {
		t.Fatalf("DialContext(%s): %v", addr, err)
	}
	t.Cleanup(func() { _ = rt.Close() })
	return rt
}

// TestClientInvokeEcho pins the unary call end to end: a request framed by the
// client is decoded by the registered method, the reply is framed back on the
// same stream, and the generated stub sees the response.
func TestClientInvokeEcho(t *testing.T) {
	addr := startServer(t, RegisterService(&api.EchoService_ServiceDesc, &echoServer{}))
	rt := dialTransport(t, addr)

	client := api.NewEchoServiceClient(rt)
	reply, err := client.Echo(context.Background(), &api.EchoRequest{Message: "hello"})
	if err != nil {
		t.Fatalf("Echo: %v", err)
	}
	if reply.Message != "hello!" {
		t.Fatalf("reply = %q, want hello!", reply.Message)
	}
}

// TestClientInvokeReportsHandlerStatus pins that the server's status error
// arrives at the caller as the same code, which is how a handler failure is
// surfaced without the connection being torn down.
func TestClientInvokeReportsHandlerStatus(t *testing.T) {
	addr := startServer(t, RegisterService(&api.EchoService_ServiceDesc, &failingEcho{}))
	rt := dialTransport(t, addr)

	err := rt.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{Message: "x"}, &api.EchoResponse{})
	if status.Code(err) != codes.InvalidArgument {
		t.Fatalf("Invoke code = %v, want InvalidArgument", status.Code(err))
	}
}

// TestClientInvokeUnimplementedMethod pins the dispatch fallback: an unknown
// method is answered with Unimplemented instead of leaving the caller waiting
// for a reply that will never come.
func TestClientInvokeUnimplementedMethod(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)

	err := rt.Invoke(context.Background(), "/nope/Nope", &api.Empty{}, &api.Empty{})
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("Invoke unknown method code = %v, want Unimplemented", status.Code(err))
	}
}

// TestClientPingSucceeds pins the health check: the server answers an empty
// request with Unimplemented, which the client counts as proof the connection is
// alive rather than as a failure.
func TestClientPingSucceeds(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)

	if err := rt.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestClientRoundTripCarriesFrames pins the framed entry point: a caller that
// marshals its own header and payload gets the matching reply back, which is the
// path the balancer and the OTel interceptor use.
func TestClientRoundTripCarriesFrames(t *testing.T) {
	addr := startServer(t, RegisterService(&api.EchoService_ServiceDesc, &echoServer{}))
	rt := dialTransport(t, addr)

	req := &api.Request{
		Options: &api.CallOptions{FullMethodName: api.EchoService_Echo_FullMethodName},
		Payload: mustMarshal(t, &api.EchoRequest{Message: "framed"}),
	}
	resp, err := rt.RoundTrip(context.Background(), req)
	if err != nil {
		t.Fatalf("RoundTrip: %v", err)
	}
	defer putmbuf(resp.Payload)

	if codes.Code(resp.Options.Code) != codes.OK {
		t.Fatalf("reply code = %v, want OK", codes.Code(resp.Options.Code))
	}
	var out api.EchoResponse
	if err := encoding.Unmarshal(resp.Payload, &out); err != nil {
		t.Fatalf("unmarshal reply: %v", err)
	}
	if out.Message != "framed!" {
		t.Fatalf("reply = %q, want framed!", out.Message)
	}
}

// mdRecorder captures what the server interceptor saw, so a test can assert the
// metadata and deadline that reached the handler.
type mdRecorder struct {
	mu           sync.Mutex
	md           metadata.MD
	deadline     time.Time
	hasDeadline  bool
	invokedCount int
}

func (r *mdRecorder) interceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		r.mu.Lock()
		r.invokedCount++
		r.md, _ = metadata.GetMetadata(ctx)
		r.deadline, r.hasDeadline = ctx.Deadline()
		r.mu.Unlock()
		return handler(ctx, req)
	}
}

func (r *mdRecorder) snapshot() (metadata.MD, time.Time, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.md, r.deadline, r.hasDeadline
}

// TestClientInvokePropagatesMetadata pins that the client's metadata travels in
// the request header and is reattached to the handler's context, which is the
// only channel an interceptor has for out-of-band values.
func TestClientInvokePropagatesMetadata(t *testing.T) {
	rec := &mdRecorder{}
	addr := startServer(t,
		RegisterService(&api.EchoService_ServiceDesc, &echoServer{}),
		UnaryServerInterceptor(rec.interceptor()),
	)
	rt := dialTransport(t, addr)

	ctx := metadata.WithMetadata(context.Background(), metadata.Pairs("trace-id", "abc123"))
	if _, err := api.NewEchoServiceClient(rt).Echo(ctx, &api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}

	md, _, _ := rec.snapshot()
	if v, ok := md.Get("trace-id"); !ok || v != "abc123" {
		t.Fatalf("server metadata = %v, want trace-id=abc123", md)
	}
}

// TestClientInvokeNarrowsTimeoutToDeadline pins the deadline rule end to end: a
// caller deadline shorter than the transport default reaches the handler as that
// deadline, because the client writes the remaining time into the header.
func TestClientInvokeNarrowsTimeoutToDeadline(t *testing.T) {
	rec := &mdRecorder{}
	addr := startServer(t,
		RegisterService(&api.EchoService_ServiceDesc, &echoServer{}),
		UnaryServerInterceptor(rec.interceptor()),
	)
	rt := dialTransport(t, addr)

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	if _, err := api.NewEchoServiceClient(rt).Echo(ctx, &api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Echo: %v", err)
	}

	_, deadline, hasDeadline := rec.snapshot()
	if !hasDeadline {
		t.Fatalf("handler context carries no deadline, want the caller's")
	}
	// The header carries milliseconds, so the bound is the caller's deadline plus
	// the rounding the truncation can introduce.
	if got := deadline.Sub(start); got > 250*time.Millisecond {
		t.Fatalf("handler deadline is %v out, want under 250ms", got)
	}
}

// TestClientCloseIsIdempotent pins the sync.Once: a second Close is a no-op and
// still reports success, so a caller that closes twice (a defer plus an explicit
// call) does not get a spurious error.
func TestClientCloseIsIdempotent(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)

	if err := rt.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rt.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestClientRefusesStreamsAfterClose pins the closed marker: with the limit
// zeroed, createStream refuses every later call instead of filling a table
// nothing will ever send from.
func TestClientRefusesStreamsAfterClose(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)
	if err := rt.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	err := rt.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("Invoke after Close = %v, want ResourceExhausted", err)
	}
}

// TestClientMaxStreamsRefusesExtraCall pins the concurrency cap: with one slot
// taken by a call that has not finished, a second call is refused rather than
// overrunning the stream table the limit exists to bound.
func TestClientMaxStreamsRefusesExtraCall(t *testing.T) {
	addr := startServer(t, RegisterService(&api.EchoService_ServiceDesc, &echoServer{}))
	rt := dialTransport(t, addr, WithMaxStreams(1))

	c := rt.(*client)
	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("first createStream: %v", err)
	}
	defer func() { _ = s.CloseSend() }()

	err = rt.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("second call = %v, want ResourceExhausted", err)
	}
}

// TestClientNewStreamUnimplemented pins that the stream surface is refused up
// front: grpcx carries unary calls only, so a caller must not be handed a stream
// that can never carry frames.
func TestClientNewStreamUnimplemented(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)

	_, err := rt.NewStream(context.Background(), &grpc.StreamDesc{}, "/nope/Nope")
	if status.Code(err) != codes.Unimplemented {
		t.Fatalf("NewStream = %v, want Unimplemented", err)
	}
}

// TestClientCreateStreamIDsIncrease pins the id allocation: every stream gets a
// fresh id, because two live calls sharing one id would deliver replies to the
// wrong caller.
func TestClientCreateStreamIDsIncrease(t *testing.T) {
	addr := startServer(t)
	rt := dialTransport(t, addr)
	c := rt.(*client)

	first, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("first createStream: %v", err)
	}
	defer func() { _ = first.CloseSend() }()
	second, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("second createStream: %v", err)
	}
	defer func() { _ = second.CloseSend() }()

	if second.id <= first.id {
		t.Fatalf("stream ids = %d then %d, want increasing", first.id, second.id)
	}
}

// TestClientDeleteStreamClosesTheSlot pins that finishing a call frees its slot
// immediately, so a stream limit of one connection does not have to wait for the
// connection to close.
func TestClientDeleteStreamClosesTheSlot(t *testing.T) {
	addr := startServer(t, RegisterService(&api.EchoService_ServiceDesc, &echoServer{}))
	rt := dialTransport(t, addr, WithMaxStreams(1))
	c := rt.(*client)

	s, err := c.createStream(context.Background())
	if err != nil {
		t.Fatalf("createStream: %v", err)
	}
	if err := s.CloseSend(); err != nil {
		t.Fatalf("CloseSend: %v", err)
	}
	// The slot is free again, so a fresh call may take it.
	if err := rt.Invoke(context.Background(), api.EchoService_Echo_FullMethodName, &api.EchoRequest{}, &api.EchoResponse{}); err != nil {
		t.Fatalf("Invoke after the slot was freed: %v", err)
	}
}

// TestServerHandleAnswersUnknownMethodRaw pins the dispatcher from the wire side:
// a header naming an unregistered method still gets a header and a payload back,
// so the connection stays usable.
func TestServerHandleAnswersUnknownMethodRaw(t *testing.T) {
	serverConn, clientConn := tcpPair(t)
	so := DefaultServerOptions
	ctx := testContext(t)

	var wg sync.WaitGroup
	wg.Go(func() {
		_ = so.Handle(ctx, serverConn)
	})
	defer func() {
		_ = clientConn.Close()
		wg.Wait()
	}()

	wire := newChannel(clientConn)
	if err := wire.Send(mustMarshal(t, &api.CallOptions{FullMethodName: "/nope/Nope", StreamID: 6})); err != nil {
		t.Fatalf("send header: %v", err)
	}
	if err := wire.Send([]byte{}); err != nil {
		t.Fatalf("send payload: %v", err)
	}

	h, err := wire.Recv()
	if err != nil {
		t.Fatalf("recv header: %v", err)
	}
	defer putmbuf(h)
	header := decodeHeader(t, h)
	if codes.Code(header.Code) != codes.Unimplemented {
		t.Fatalf("reply code = %v, want Unimplemented", codes.Code(header.Code))
	}
	if header.StreamID != 6 {
		t.Fatalf("reply StreamID = %d, want 6", header.StreamID)
	}
	p, err := wire.Recv()
	if err != nil {
		t.Fatalf("recv payload: %v", err)
	}
	putmbuf(p)
}

// TestServerHandleReturnsWhenPeerCloses pins that a connection ending is
// reported, not swallowed: the caller of Handle is the one that closes the
// connection and logs the exit.
func TestServerHandleReturnsWhenPeerCloses(t *testing.T) {
	serverConn, clientConn := tcpPair(t)
	so := DefaultServerOptions

	done := make(chan error, 1)
	go func() { done <- so.Handle(context.Background(), serverConn) }()

	if err := clientConn.Close(); err != nil {
		t.Fatalf("close peer: %v", err)
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("Handle = nil after the peer closed, want the connection error")
		}
	case <-time.After(time.Second):
		t.Fatal("Handle did not return after the peer closed")
	}
}

// TestServerReadTimeoutClosesIdleConnection pins that WithServerReadTimeout
// reaches the connection: a peer that connects and never sends a frame is cut
// off once the read deadline expires, instead of pinning a handler forever.
func TestServerReadTimeoutClosesIdleConnection(t *testing.T) {
	addr := startServer(t,
		RegisterService(&api.EchoService_ServiceDesc, &echoServer{}),
		WithServerReadTimeout(50*time.Millisecond),
	)

	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	defer func() { _ = conn.Close() }()
	_ = conn.SetReadDeadline(time.Now().Add(2 * time.Second))

	// No frame is written, so only the server's own read timeout can end this:
	// the close it performs shows up here as EOF well before the 2s deadline.
	buf := make([]byte, 8)
	var nerr net.Error
	if _, err := conn.Read(buf); err == nil {
		t.Fatal("read on an idle connection succeeded, want the server to close it")
	} else if errors.As(err, &nerr) && nerr.Timeout() {
		t.Fatal("the client's own 2s deadline fired, want the server's 50ms read timeout first")
	}
}

package transport

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// echoImpl is the service every end-to-end test registers: it answers with the
// request message plus "!", and fails the call whose message matches failOn so a
// handler error can be observed without the connection going away.
type echoImpl struct {
	api.UnimplementedEchoServiceServer

	failOn string
	panics bool
	delay  time.Duration
}

// Echo implements api.EchoServiceServer.
func (e *echoImpl) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
	if e.delay > 0 {
		time.Sleep(e.delay)
	}
	if e.panics {
		panic("test: handler panicked")
	}
	if e.failOn != "" && req.Message == e.failOn {
		return nil, status.Error(codes.InvalidArgument, "test: handler failed")
	}
	return &api.EchoResponse{Message: req.Message + "!"}, nil
}

// fakeSender records every frame it is handed, flattened in call order, and
// counts its closes. It satisfies SendCloser, so it can stand in for a channel.
type fakeSender struct {
	mu     sync.Mutex
	frames [][]byte
	err    error
	closes int
}

// Send implements Sender.
func (f *fakeSender) Send(p ...[]byte) error {
	if f.err != nil {
		return f.err
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, b := range p {
		f.frames = append(f.frames, append([]byte(nil), b...))
	}
	return nil
}

// Close implements io.Closer.
func (f *fakeSender) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeSender) sent() [][]byte {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([][]byte{}, f.frames...)
}

// fakeRecver hands out its frames in order, then err (io.EOF when unset). Its
// frames are Buffers because every reader gives back what it read with Free.
type fakeRecver struct {
	mu     sync.Mutex
	frames []Buffer
	err    error
}

func (f *fakeRecver) Recv() (Buffer, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.frames) == 0 {
		if f.err != nil {
			return nil, f.err
		}
		return nil, io.EOF
	}
	p := f.frames[0]
	f.frames = f.frames[1:]
	return p, nil
}

func (f *fakeRecver) remaining() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.frames)
}

// fakeChannel is a Channel over a fakeSender and a fakeRecver, so a server
// interceptor can be driven without a socket.
type fakeChannel struct {
	fakeSender
	recver fakeRecver
}

func (c *fakeChannel) Recv() (Buffer, error) { return c.recver.Recv() }

// closeOnCleanup closes every connection when the test ends.
func closeOnCleanup(t *testing.T, conns ...net.Conn) {
	t.Helper()
	t.Cleanup(func() {
		for _, c := range conns {
			_ = c.Close()
		}
	})
}

// fixedCodec returns the same bytes for every value, which makes it
// distinguishable from the built-in proto codec on the wire.
type fixedCodec struct {
	out []byte
	err error
}

// Name implements encoding.Codec.
func (f fixedCodec) Name() string { return "fixed" }

// Marshal implements encoding.Codec.
func (f fixedCodec) Marshal(any) ([]byte, error) { return f.out, f.err }

// Unmarshal implements encoding.Codec.
func (f fixedCodec) Unmarshal([]byte, any) error { return nil }

// pooledPayload copies b into a pooled buffer, the way the channel hands a frame
// to its reader.
func pooledPayload(b []byte) defaultBuffer {
	p := getmbuf(len(b))
	copy(p, b)
	return p[:len(b)]
}

// countingBuffer is a Buffer that counts its Free calls, so a test can prove the
// side that took a frame off a connection gave it back exactly once.
type countingBuffer struct {
	data  []byte
	mu    sync.Mutex
	freed int
}

// Free implements Buffer.
func (b *countingBuffer) Free() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.freed++
}

// ReadOnlyData implements Buffer.
func (b *countingBuffer) ReadOnlyData() []byte { return b.data }

func (b *countingBuffer) freeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.freed
}

func countingPayload(b []byte) *countingBuffer {
	return &countingBuffer{data: append([]byte(nil), b...)}
}

// mustMarshal marshals m with the package codec.
func mustMarshal(t *testing.T, m any) []byte {
	t.Helper()
	b, err := encoding.Marshal(m)
	if err != nil {
		t.Fatalf("marshal %T: %v", m, err)
	}
	return b
}

// headerFrame is a request header ready to be read by fakeRecver.
func headerFrame(t *testing.T, opts *api.CallOptions) defaultBuffer {
	t.Helper()
	return pooledPayload(mustMarshal(t, opts))
}

// payloadFrame is a message body ready to be read by fakeRecver.
func payloadFrame(t *testing.T, m any) defaultBuffer {
	t.Helper()
	return pooledPayload(mustMarshal(t, m))
}

// newTestClient builds a client on conn and starts its receive loop, so a call
// can be made against a hand-written server end.
func newTestClient(t *testing.T, conn net.Conn, opts ...Option) *client {
	t.Helper()
	ch := newChannel(conn, opts...)
	o := DefaultOptions()
	for _, v := range opts {
		v(&o)
	}
	c := &client{
		SendCloser: ch,
		Options:    o,
		streams:    make(map[uint32]*clientStream),
		Codec:      o.newCodec(),
	}
	ctx, stop := context.WithCancel(context.Background())
	go func() { _ = c.Handle(ctx, ch) }()
	t.Cleanup(func() {
		stop()
		_ = c.Close()
	})
	return c
}

// startPipeServer serves one net.Pipe connection through ServerOptions.Handle
// and dials a client to the other end. handleErr receives what Handle returns
// once the connection ends.
func startPipeServer(t *testing.T, opts ...ServerOption) (cli *client, cancel context.CancelFunc, handleErr <-chan error) {
	t.Helper()
	cliConn, srvConn := net.Pipe()
	so := DefaultServerOptions()
	for _, v := range opts {
		v(&so)
	}
	ctx, stop := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() { errs <- so.Handle(ctx, srvConn) }()
	cli = newTestClient(t, cliConn)
	t.Cleanup(stop)
	return cli, stop, errs
}

// echoService registers the generated Echo service, the shape a real service
// descriptor arrives in.
func echoService(impl api.EchoServiceServer) ServerOption {
	return RegisterService(&api.EchoService_ServiceDesc, impl)
}

// serverOptions builds a ServerOptions with every option applied, and the
// channel that stands in for the accepted connection.
func serverOptions(opts ...ServerOption) ServerOptions {
	so := DefaultServerOptions()
	for _, v := range opts {
		v(&so)
	}
	return so
}

// newStreamUnderTest builds a clientStream over a sender, with a cancellable
// context and a counter for its deregistrations.
func newStreamUnderTest(sender SendCloser) (*clientStream, *atomic.Int32) {
	ctx, cancel := context.WithCancel(context.Background())
	var deleted atomic.Int32
	s := &clientStream{
		ctx:    ctx,
		id:     7,
		c:      make(chan *reply, 1),
		cancel: cancel,
		deleteStream: func(uint32) {
			deleted.Add(1)
		},
		Sender: sender,
	}
	return s, &deleted
}

// answeringSender stands in for the client connection: it reads the stream id
// off the request header and answers through respond, so Invoke and RoundTrip
// can be exercised without a socket.
type answeringSender struct {
	mu      sync.Mutex
	c       *client
	frames  [][]byte
	closes  int
	respond func(s *clientStream, header *api.CallOptions, payload []byte) error
}

// Send implements Sender.
func (a *answeringSender) Send(p ...[]byte) error {
	if len(p) != 2 {
		return io.ErrShortWrite
	}
	var header api.CallOptions
	if err := encoding.Unmarshal(p[0], &header); err != nil {
		return err
	}
	a.mu.Lock()
	a.frames = append(a.frames, append([]byte(nil), p[0]...), append([]byte(nil), p[1]...))
	a.mu.Unlock()
	s := a.c.getStream(header.StreamID)
	if s == nil {
		return status.Unavailable.Err()
	}
	if a.respond == nil {
		return nil
	}
	return a.respond(s, &header, p[1])
}

// Close implements io.Closer.
func (a *answeringSender) Close() error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.closes++
	return nil
}

func (a *answeringSender) sent() [][]byte {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([][]byte{}, a.frames...)
}

func (a *answeringSender) closeCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.closes
}

// newClientOnSender builds a client whose connection is an answeringSender,
// with the respond hook installed before the client is handed out.
func newClientOnSender(t *testing.T, respond func(*clientStream, *api.CallOptions, []byte) error, opts ...Option) (*client, *answeringSender) {
	t.Helper()
	o := DefaultOptions()
	for _, v := range opts {
		v(&o)
	}
	a := &answeringSender{respond: respond}
	c := &client{
		Options: o,
		streams: make(map[uint32]*clientStream),
		Codec:   o.newCodec(),
	}
	c.SendCloser = a
	a.c = c
	t.Cleanup(func() { _ = c.Close() })
	return c, a
}

// replyOnSlot is the respond hook most tests want: it fills the stream's receive
// slot exactly the way clientStream.Handle does once the frame is off the wire.
func replyOnSlot(code codes.Code, desc string) func(*clientStream, *api.CallOptions, []byte) error {
	return func(s *clientStream, _ *api.CallOptions, payload []byte) error {
		s.c <- &reply{
			CallOption: WithCallOptions(&api.CallOptions{Code: uint32(code), Desc: desc}),
			Buffer:     pooledPayload(payload),
		}
		return nil
	}
}

// replyEcho answers the way the echo service does, so a call can be handed a
// failing status through the same hook.
func replyEcho(t *testing.T, code codes.Code, desc string) func(*clientStream, *api.CallOptions, []byte) error {
	t.Helper()
	return func(s *clientStream, _ *api.CallOptions, payload []byte) error {
		var req api.EchoRequest
		if err := encoding.Unmarshal(payload, &req); err != nil {
			return err
		}
		body := mustMarshal(t, &api.EchoResponse{Message: req.Message + "!"})
		s.c <- &reply{
			CallOption: WithCallOptions(&api.CallOptions{Code: uint32(code), Desc: desc}),
			Buffer:     pooledPayload(body),
		}
		return nil
	}
}

var _ grpc.ClientStream = (*clientStream)(nil)

// joinPairs renders a flat metadata slice so a test can compare it in one
// expression.
func joinPairs(pairs []string) string {
	var b strings.Builder
	for i := 0; i+1 < len(pairs); i += 2 {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteString(pairs[i])
		b.WriteByte('=')
		b.WriteString(pairs[i+1])
	}
	return b.String()
}

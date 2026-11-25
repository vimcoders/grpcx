package transport

import (
	"context"
	"io"
	"net"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

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
}

// Echo implements api.EchoServiceServer.
func (e *echoImpl) Echo(_ context.Context, req *api.EchoRequest) (*api.EchoResponse, error) {
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
// frames are pooled buffers because every reader returns what it reads with
// putmbuf.
type fakeRecver struct {
	mu     sync.Mutex
	frames [][]byte
	err    error
}

func (f *fakeRecver) Recv() ([]byte, error) {
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

// countingCodec records how often it was used, so a test can prove the codec
// option reached the call path and not only the options struct.
type countingCodec struct {
	marshal   atomic.Int32
	unmarshal atomic.Int32
}

func (c *countingCodec) Name() string { return "counting" }

func (c *countingCodec) Marshal(msg any) ([]byte, error) {
	c.marshal.Add(1)
	return encoding.Marshal(msg)
}

func (c *countingCodec) Unmarshal(p []byte, msg any) error {
	c.unmarshal.Add(1)
	return encoding.Unmarshal(p, msg)
}

// pooledPayload copies b into a pooled buffer, the way the channel hands a frame
// to its reader.
func pooledPayload(b []byte) []byte {
	p := getmbuf(len(b))
	copy(p[:len(b)], b)
	return p[:len(b)]
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

// headerFrame is a header ready to be read by fakeRecver.
func headerFrame(t *testing.T, opts *api.CallOptions) []byte {
	t.Helper()
	return pooledPayload(mustMarshal(t, opts))
}

// payloadFrame is a payload ready to be read by fakeRecver.
func payloadFrame(t *testing.T, m any) []byte {
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
		// Close calls cancel unconditionally; a hand-built client is never wired
		// to DialContext, which is where the real cancel is installed.
		cancel: func() {},
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
	so := DefaultServerOptions
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

// newStreamUnderTest builds a clientStream over the frame pool, with a
// cancellable context and a counter for its deregistrations.
func newStreamUnderTest(sender SendCloser) (*clientStream, *atomic.Int32) {
	ctx, cancel := context.WithCancel(context.Background())
	var deleted atomic.Int32
	s := &clientStream{
		ctx:    ctx,
		id:     7,
		c:      make(chan *api.Response, 1),
		cancel: cancel,
		deleteStream: func(uint32) {
			deleted.Add(1)
		},
		codec:  encoding.GetCodec(encoding.Name),
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
	// cancel stands in for the receive loop's cancel: a hand-built client is never
	// wired to DialContext, and Close calls it unconditionally.
	c := &client{Options: o, streams: make(map[uint32]*clientStream), cancel: func() {}}
	c.SendCloser = a
	a.c = c
	t.Cleanup(func() { _ = c.Close() })
	return c, a
}

// replyOnSlot is the respond hook most tests want: it fills the stream's receive
// slot exactly the way clientStream.Handle does once the frame is off the wire.
func replyOnSlot(code codes.Code, desc string) func(*clientStream, *api.CallOptions, []byte) error {
	return func(s *clientStream, _ *api.CallOptions, payload []byte) error {
		reply := make([]byte, len(payload))
		copy(reply, payload)
		s.c <- &api.Response{
			Options: &api.CallOptions{Code: uint32(code), Desc: desc},
			Payload: reply,
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
		s.c <- &api.Response{
			Options: &api.CallOptions{Code: uint32(code), Desc: desc},
			Payload: body,
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

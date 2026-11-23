package balancer

import (
	"context"
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
)

// fakeRT is a RoundTripper that only records what the balancer does to it: a
// balancer never carries a call itself, so RoundTrip, Invoke and NewStream stay
// unimplemented stubs.
type fakeRT struct {
	mu      sync.Mutex
	pingErr error
	pings   int
	closed  int
}

var errNotUsed = errors.New("fakeRT: call path not used by the balancer")

func (r *fakeRT) RoundTrip(context.Context, *api.Request) (*api.Response, error) {
	return nil, errNotUsed
}

func (r *fakeRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return errNotUsed
}

func (r *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, errNotUsed
}

func (r *fakeRT) Ping(context.Context) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.pings++
	return r.pingErr
}

func (r *fakeRT) Close() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.closed++
	return nil
}

func (r *fakeRT) stats() (pings, closed int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pings, r.closed
}

// TestPickRotatesEveryBackend checks the rotation: the first pick lands on the
// first backend and the following picks visit every backend in order.
func TestPickRotatesEveryBackend(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{}, &fakeRT{}, &fakeRT{}}
	rr := RoundRobin{rts: rts}

	for i := 0; i < 2*len(rts); i++ {
		got, err := rr.Pick(context.Background(), PickInfo{FullMethodName: "/api.EchoService/Echo"})
		if err != nil {
			t.Fatalf("pick %d = %v", i, err)
		}
		want := rts[i%len(rts)]
		if got != want {
			t.Fatalf("pick %d returned backend %d, want %d", i, indexOf(rts, got), indexOf(rts, want))
		}
	}
}

func indexOf(rts []transport.RoundTripper, rt transport.RoundTripper) int {
	for i, v := range rts {
		if v == rt {
			return i
		}
	}
	return -1
}

func TestPickWithoutBackends(t *testing.T) {
	rr := RoundRobin{}
	_, err := rr.Pick(context.Background(), PickInfo{})
	if err == nil {
		t.Fatalf("Pick on an empty balancer = nil error")
	}
	if got := status.Code(err); got != status.ResourceExhausted.Code() {
		t.Fatalf("Pick error code = %v, want ResourceExhausted", got)
	}
}

// TestPickConcurrent hammers Pick from several goroutines; with -race this also
// checks the read lock around the backend slice.
func TestPickConcurrent(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{}, &fakeRT{}}
	rr := RoundRobin{rts: rts}

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := rr.Pick(context.Background(), PickInfo{}); err != nil {
					t.Errorf("concurrent Pick = %v", err)
					return
				}
			}
		})
	}
	wg.Wait()
}

// TestPingTrimsExtraBackends keeps the first podCount healthy backends and
// closes the rest.
func TestPingTrimsExtraBackends(t *testing.T) {
	a, b, c := &fakeRT{}, &fakeRT{}, &fakeRT{}
	dials := 0
	rr := RoundRobin{
		rts:            []transport.RoundTripper{a, b, c},
		resolveContext: func(context.Context) (int, error) { return 2, nil },
		dialContext: func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{}, nil
		},
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v", err)
	}

	if dials != 0 {
		t.Fatalf("dialed %d backends, want 0: the pool was already large enough", dials)
	}
	if _, closed := c.stats(); closed != 1 {
		t.Fatalf("extra backend closed %d times, want 1", closed)
	}
	got := rr.clone()
	if len(got) != 2 || got[0] != a || got[1] != b {
		t.Fatalf("backends after ping = %d, want [a b]", len(got))
	}
}

// TestPingReplacesDeadBackend covers the failure half: a backend whose ping
// fails is closed, and a fresh dial fills the gap.
func TestPingReplacesDeadBackend(t *testing.T) {
	a := &fakeRT{}
	dead := &fakeRT{pingErr: errors.New("connection reset")}
	dialed := &fakeRT{}
	dials := 0
	rr := RoundRobin{
		rts:            []transport.RoundTripper{a, dead},
		resolveContext: func(context.Context) (int, error) { return 2, nil },
		dialContext: func(context.Context) (transport.RoundTripper, error) {
			dials++
			return dialed, nil
		},
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v", err)
	}

	if _, closed := dead.stats(); closed != 1 {
		t.Fatalf("dead backend closed %d times, want 1", closed)
	}
	if dials != 1 {
		t.Fatalf("dialed %d times, want 1", dials)
	}
	got := rr.clone()
	if len(got) != 2 || got[0] != a || got[1] != dialed {
		t.Fatalf("backends after ping = %d entries, want [a dialed]", len(got))
	}
}

func TestPingGrowsPool(t *testing.T) {
	dials := 0
	rr := RoundRobin{
		resolveContext: func(context.Context) (int, error) { return 3, nil },
		dialContext: func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{}, nil
		},
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v", err)
	}
	if dials != 3 {
		t.Fatalf("dialed %d times, want 3", dials)
	}
	if got := len(rr.clone()); got != 3 {
		t.Fatalf("pool = %d backends, want 3", got)
	}
}

// TestPingSkipsFailedDials checks that a dial failure leaves the pool short
// rather than failing the whole ping.
func TestPingSkipsFailedDials(t *testing.T) {
	rr := RoundRobin{
		resolveContext: func(context.Context) (int, error) { return 2, nil },
		dialContext: func(context.Context) (transport.RoundTripper, error) {
			return nil, errors.New("dial refused")
		},
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping = %v, want nil: a failed dial is not a ping failure", err)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool = %d backends, want 0", got)
	}
}

func TestPingPropagatesResolveError(t *testing.T) {
	resolveErr := errors.New("no such host")
	a := &fakeRT{}
	rr := RoundRobin{
		rts:            []transport.RoundTripper{a},
		resolveContext: func(context.Context) (int, error) { return 0, resolveErr },
	}
	if err := rr.Ping(context.Background()); !errors.Is(err, resolveErr) {
		t.Fatalf("Ping = %v, want %v", err, resolveErr)
	}
	if pings, _ := a.stats(); pings != 0 {
		t.Fatalf("backend pinged %d times after a resolve failure, want 0", pings)
	}
}

func TestKeepaliveStopsOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	rr := RoundRobin{resolveContext: func(context.Context) (int, error) { return 0, nil }}
	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx) }()

	select {
	case err := <-done:
		if got := status.Code(err); got != status.Canceled.Code() {
			t.Fatalf("Keepalive = %v (code %v), want Canceled", err, got)
		}
	case <-time.After(5 * time.Second):
		t.Fatalf("Keepalive did not return after the context was canceled")
	}
}

func TestCloseCancelsContext(t *testing.T) {
	called := false
	rr := RoundRobin{cancel: func() { called = true }}
	if err := rr.Close(); err != nil {
		t.Fatalf("Close = %v", err)
	}
	if !called {
		t.Fatalf("Close did not cancel the balancer context")
	}

	// A zero value has nothing to cancel and must not panic.
	if err := (&RoundRobin{}).Close(); err != nil {
		t.Fatalf("zero RoundRobin Close = %v", err)
	}
}

// TestBuildReportsEmptyEndpoint checks the resolver error path: an empty host
// cannot be resolved, so Build fails before dialing anything.
func TestBuildReportsEmptyEndpoint(t *testing.T) {
	var b rrBuilder
	if _, err := b.Build(context.Background(), ""); err == nil {
		t.Fatalf("Build(\"\") = nil error")
	}
}

// TestDialContextConnects drives the real path against a loopback listener: the
// endpoint resolves through the dns resolver, one connection is dialed and the
// picker returns it.
func TestDialContextConnects(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	conns := make(chan net.Conn, 4)
	go func() {
		for {
			c, acceptErr := ln.Accept()
			if acceptErr != nil {
				return
			}
			conns <- c
		}
	}()

	picker, err := DialContext(context.Background(), ln.Addr().String())
	if err != nil {
		_ = ln.Close()
		t.Fatalf("DialContext(%s) = %v", ln.Addr(), err)
	}
	t.Cleanup(func() {
		_ = picker.Close()
		_ = ln.Close()
		for {
			select {
			case c := <-conns:
				_ = c.Close()
			default:
				return
			}
		}
	})

	rt, err := picker.Pick(context.Background(), PickInfo{FullMethodName: "/api.EchoService/Echo"})
	if err != nil {
		t.Fatalf("Pick = %v", err)
	}
	if rt == nil {
		t.Fatalf("Pick returned a nil backend")
	}
}

// TestDialContextRefused checks that a refused connection surfaces from
// DialContext instead of returning a picker with no backends.
func TestDialContextRefused(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()

	if _, err := DialContext(context.Background(), addr); err == nil {
		t.Fatalf("DialContext on a closed port = nil error")
	}
}

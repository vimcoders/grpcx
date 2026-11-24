package balancer

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/resolver"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// The builder and the picker both satisfy their interfaces, so a signature
// drift breaks the test build instead of the call site.
var (
	_ Builder = (*rrBuilder)(nil)
	_ Picker  = (*RoundRobin)(nil)
)

// fakeRT is a transport.RoundTripper that answers ping with a fixed result and
// counts how often it was pinged and closed.
type fakeRT struct {
	name    string
	pingErr error
	pings   atomic.Int32
	closes  atomic.Int32
}

func (f *fakeRT) RoundTrip(context.Context, *api.Request) (*api.Response, error) {
	return nil, nil
}

func (f *fakeRT) Ping(context.Context) error {
	f.pings.Add(1)
	return f.pingErr
}

func (f *fakeRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return nil
}

func (f *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, nil
}

func (f *fakeRT) Close() error {
	f.closes.Add(1)
	return nil
}

func (f *fakeRT) closeCount() int { return int(f.closes.Load()) }

func (f *fakeRT) pingCount() int { return int(f.pings.Load()) }

// podAddrs returns n synthetic addresses. grpcx does not dial per address: the
// number of addresses only sizes the pool, and K8s decides which pod the
// connection lands on.
func podAddrs(n int) []resolver.Address {
	addrs := make([]resolver.Address, n)
	for i := range addrs {
		addrs[i] = resolver.Address{Addr: fmt.Sprintf("10.0.0.%d:50051", i+1)}
	}
	return addrs
}

// newTestRoundRobin builds a RoundRobin without the DNS/dial closures of
// DialContext, so a test can inject its own resolver and dialer.
func newTestRoundRobin(rts []transport.RoundTripper, addrs []resolver.Address, dial func(context.Context) (transport.RoundTripper, error)) *RoundRobin {
	if dial == nil {
		dial = func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil }
	}
	return &RoundRobin{
		rts: rts,
		resolve: func(context.Context) ([]resolver.Address, error) {
			return addrs, nil
		},
		dial: dial,
	}
}

func statusCodeOf(err error) codes.Code {
	return status.Code(err)
}

// TestRoundRobinPickRotates pins the rotation: step starts at zero, is
// incremented before the modulo, so the first pick lands on index 1 and the
// sequence over three backends is b, c, a, b.
func TestRoundRobinPickRotates(t *testing.T) {
	a, b, c := &fakeRT{name: "a"}, &fakeRT{name: "b"}, &fakeRT{name: "c"}
	rr := newTestRoundRobin([]transport.RoundTripper{a, b, c}, podAddrs(3), nil)

	want := []string{"b", "c", "a", "b"}
	for i, name := range want {
		rt, err := rr.Pick(t.Context(), PickInfo{})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if got := rt.(*fakeRT).name; got != name {
			t.Fatalf("pick %d = %q, want %q", i, got, name)
		}
	}
}

// TestRoundRobinPickWithoutBackends pins the error for an empty pool: every
// backend was closed or never dialed, so the caller gets a retryable code
// instead of a nil round tripper.
func TestRoundRobinPickWithoutBackends(t *testing.T) {
	rr := newTestRoundRobin(nil, podAddrs(2), nil)

	rt, err := rr.Pick(t.Context(), PickInfo{})
	if statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty pool = %v, want ResourceExhausted", err)
	}
	if rt != nil {
		t.Fatalf("Pick on an empty pool = %v, want nil round tripper", rt)
	}
}

// TestRoundRobinCloseClosesEveryBackend pins that Close closes each backend
// exactly once and reports no error even when a backend's own close fails.
func TestRoundRobinCloseClosesEveryBackend(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{name: "a"}, &fakeRT{name: "b"}}
	rr := newTestRoundRobin(rts, podAddrs(2), nil)

	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for _, rt := range rts {
		if got := rt.(*fakeRT).closeCount(); got != 1 {
			t.Fatalf("backend %s closed %d times, want 1", rt.(*fakeRT).name, got)
		}
	}
}

// TestRoundRobinCloseIsRepeatable pins that Close is safe twice: the second
// call re-closes the same backends rather than failing or panicking, which
// covers the shutdown path that runs after an earlier close.
func TestRoundRobinCloseIsRepeatable(t *testing.T) {
	rt := &fakeRT{name: "a"}
	rr := newTestRoundRobin([]transport.RoundTripper{rt}, podAddrs(1), nil)

	if err := rr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := rt.closeCount(); got != 2 {
		t.Fatalf("backend closed %d times, want 2", got)
	}
}

// TestRoundRobinPickAfterClose pins that a closed pool keeps serving picks from
// its snapshot: Close does not clear the slice, so the caller must stop using
// the picker itself when it shuts down.
func TestRoundRobinPickAfterClose(t *testing.T) {
	rt := &fakeRT{name: "a"}
	rr := newTestRoundRobin([]transport.RoundTripper{rt}, podAddrs(1), nil)

	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := rr.Pick(t.Context(), PickInfo{}); err != nil {
		t.Fatalf("Pick after Close: %v", err)
	}
}

// TestRoundRobinKeepaliveStopsOnCancel pins the keepalive contract: it blocks
// until ctx is canceled and then returns Canceled, so the goroutine Build
// starts ends with the balancer's context.
func TestRoundRobinKeepaliveStopsOnCancel(t *testing.T) {
	rr := newTestRoundRobin([]transport.RoundTripper{&fakeRT{name: "a"}}, podAddrs(1), nil)
	ctx, cancel := context.WithCancel(t.Context())

	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx) }()
	cancel()

	select {
	case err := <-done:
		if statusCodeOf(err) != codes.Canceled {
			t.Fatalf("Keepalive after cancel = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Keepalive did not return within 1s of cancel")
	}
}

// TestRoundRobinPingDropsUnresponsiveBackend pins the health pass: a backend
// whose ping fails is closed, and a replacement is dialed so the pool keeps the
// size of the resolution.
func TestRoundRobinPingDropsUnresponsiveBackend(t *testing.T) {
	dead := &fakeRT{name: "dead", pingErr: status.Unavailable.Err()}
	alive := &fakeRT{name: "alive"}
	dials := 0
	rr := newTestRoundRobin(
		[]transport.RoundTripper{dead, alive},
		podAddrs(2),
		func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{name: fmt.Sprintf("rt%d", dials)}, nil
		},
	)

	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := dead.closeCount(); got != 1 {
		t.Fatalf("unresponsive backend closed %d times, want 1", got)
	}
	if got := alive.closeCount(); got != 0 {
		t.Fatalf("responsive backend closed %d times, want 0", got)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool size after ping = %d, want 2", got)
	}
	if dials != 1 {
		t.Fatalf("dialed %d replacements, want 1", dials)
	}
}

// TestRoundRobinPingClosesExcessBackends pins the shrink path: when the
// resolution returns fewer addresses than the pool holds, the extra backends
// are closed until the pool matches the resolution.
func TestRoundRobinPingClosesExcessBackends(t *testing.T) {
	a, b, c := &fakeRT{name: "a"}, &fakeRT{name: "b"}, &fakeRT{name: "c"}
	rr := newTestRoundRobin([]transport.RoundTripper{a, b, c}, podAddrs(2), nil)

	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool size after ping = %d, want 2", got)
	}
	closed := 0
	for _, rt := range []*fakeRT{a, b, c} {
		closed += rt.closeCount()
	}
	if closed != 1 {
		t.Fatalf("closed %d excess backends, want 1", closed)
	}
}

// TestRoundRobinPingRedialsToAddressCount pins the grow path: when the
// resolution returns more addresses than the pool holds, Ping dials the
// difference rather than rebuilding the whole pool.
func TestRoundRobinPingRedialsToAddressCount(t *testing.T) {
	first := &fakeRT{name: "first"}
	dials := 0
	rr := newTestRoundRobin(
		[]transport.RoundTripper{first},
		podAddrs(3),
		func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{name: fmt.Sprintf("rt%d", dials)}, nil
		},
	)

	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 3 {
		t.Fatalf("pool size after ping = %d, want 3", got)
	}
	if dials != 2 {
		t.Fatalf("dialed %d replacements, want 2", dials)
	}
	if got := first.closeCount(); got != 0 {
		t.Fatalf("surviving backend closed %d times, want 0", got)
	}
	if got := first.pingCount(); got != 1 {
		t.Fatalf("surviving backend pinged %d times, want 1", got)
	}
}

// TestRoundRobinPingReportsResolveError pins that a failing resolution aborts
// the pass before anything is closed, so a DNS hiccup never tears down a
// healthy pool.
func TestRoundRobinPingReportsResolveError(t *testing.T) {
	boom := errors.New("resolver: no addresses found")
	rts := []transport.RoundTripper{&fakeRT{name: "a", pingErr: status.Unavailable.Err()}}
	rr := &RoundRobin{
		rts:     rts,
		resolve: func(context.Context) ([]resolver.Address, error) { return nil, boom },
		dial:    func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil },
	}

	if err := rr.Ping(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Ping = %v, want %v", err, boom)
	}
	if got := rts[0].(*fakeRT).closeCount(); got != 0 {
		t.Fatalf("backend closed %d times on a resolve error, want 0", got)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool size after a failed resolve = %d, want 1", got)
	}
}

// TestRoundRobinPingSkipsUndialableReplacement pins the best-effort refill: a
// replacement that fails to dial is counted as one dial, leaves the pool short
// and does not turn into an error, so one bad pod cannot fail the whole pass.
func TestRoundRobinPingSkipsUndialableReplacement(t *testing.T) {
	dials := 0
	rr := newTestRoundRobin(
		[]transport.RoundTripper{&fakeRT{name: "a"}},
		podAddrs(3),
		func(context.Context) (transport.RoundTripper, error) {
			dials++
			if dials == 1 {
				return nil, errors.New("dial failed")
			}
			return &fakeRT{name: fmt.Sprintf("rt%d", dials)}, nil
		},
	)

	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool size after a failed replacement = %d, want 2", got)
	}
	if dials != 2 {
		t.Fatalf("dialed %d times, want 2", dials)
	}
}

// TestRoundRobinPingWithEmptyResolutionClosesBackends pins what an empty
// resolution does: every backend is closed and the pool empties, so the next
// pick reports ResourceExhausted until a later pass fills it again.
func TestRoundRobinPingWithEmptyResolutionClosesBackends(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{name: "a"}, &fakeRT{name: "b"}}
	rr := newTestRoundRobin(rts, nil, nil)

	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool size after an empty resolution = %d, want 0", got)
	}
	for _, rt := range rts {
		if got := rt.(*fakeRT).closeCount(); got != 1 {
			t.Fatalf("backend %s closed %d times, want 1", rt.(*fakeRT).name, got)
		}
	}
	if _, err := rr.Pick(t.Context(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick after an empty resolution = %v, want ResourceExhausted", err)
	}
}

// TestBuildWithoutAddressIsUnavailable pins that Build refuses an empty
// resolution: dialing zero backends would hand the caller a pool that can only
// fail picks.
func TestBuildWithoutAddressIsUnavailable(t *testing.T) {
	dials := 0
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{}, nil
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return nil, nil },
	}

	_, err := b.Build(t.Context(), "example.com:50051")
	if statusCodeOf(err) != codes.Unavailable {
		t.Fatalf("Build on an empty resolution = %v, want Unavailable", err)
	}
	if dials != 0 {
		t.Fatalf("dialed %d backends on an empty resolution, want 0", dials)
	}
}

// TestBuildPropagatesResolveError pins that Build returns the resolver error as
// is, so the caller sees the DNS failure instead of a generic status.
func TestBuildPropagatesResolveError(t *testing.T) {
	boom := errors.New("resolver: no addresses found")
	b := rrBuilder{
		dial:    func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil },
		resolve: func(context.Context) ([]resolver.Address, error) { return nil, boom },
	}

	if _, err := b.Build(t.Context(), "example.com:50051"); !errors.Is(err, boom) {
		t.Fatalf("Build = %v, want %v", err, boom)
	}
}

// TestBuildDialsOneBackendPerAddress pins the pool sizing: one connection per
// resolved address, and the returned picker serves picks from that pool.
func TestBuildDialsOneBackendPerAddress(t *testing.T) {
	dials := 0
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials++
			return &fakeRT{name: fmt.Sprintf("rt%d", dials)}, nil
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return podAddrs(3), nil },
	}

	picker, err := b.Build(t.Context(), "example.com:50051")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = picker.Close() }()

	if dials != 3 {
		t.Fatalf("dialed %d backends, want 3", dials)
	}
	rt, err := picker.Pick(t.Context(), PickInfo{})
	if err != nil {
		t.Fatalf("Pick after Build: %v", err)
	}
	if rt == nil {
		t.Fatal("Pick after Build = nil round tripper")
	}
}

// TestBuildClosesHalfBuiltBackendsOnDialError pins the failure path: the
// backends dialed before the failure are closed before Build returns, so a
// partial pool does not leak connections.
func TestBuildClosesHalfBuiltBackendsOnDialError(t *testing.T) {
	first := &fakeRT{name: "first"}
	boom := errors.New("dial failed")
	dials := 0
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials++
			if dials == 1 {
				return first, nil
			}
			return nil, boom
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return podAddrs(2), nil },
	}

	if _, err := b.Build(t.Context(), "example.com:50051"); !errors.Is(err, boom) {
		t.Fatalf("Build = %v, want %v", err, boom)
	}
	if got := first.closeCount(); got != 1 {
		t.Fatalf("half-built backend closed %d times, want 1", got)
	}
}

// TestRoundRobinConcurrentPickAndClose pins that picks racing Close are safe:
// Close snapshots the slice, so a pick either sees the full slice or a copy of
// it, and never reads a slice being replaced.
func TestRoundRobinConcurrentPickAndClose(t *testing.T) {
	a, b, c := &fakeRT{name: "a"}, &fakeRT{name: "b"}, &fakeRT{name: "c"}
	rr := newTestRoundRobin([]transport.RoundTripper{a, b, c}, podAddrs(3), nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 100 {
				if _, err := rr.Pick(t.Context(), PickInfo{}); err != nil {
					t.Errorf("Pick during Close: %v", err)
					return
				}
			}
		})
	}
	wg.Go(func() { _ = rr.Close() })
	wg.Wait()
}

// TestRoundRobinConcurrentPingAndPick pins that a ping swapping the pool races
// picks and closes safely: the table is swapped under the write lock, and
// readers either see the old or the new snapshot.
func TestRoundRobinConcurrentPingAndPick(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{name: "a"}, &fakeRT{name: "b"}}
	rr := newTestRoundRobin(rts, podAddrs(2), nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 50 {
				if _, err := rr.Pick(t.Context(), PickInfo{}); err != nil {
					t.Errorf("Pick during Ping: %v", err)
					return
				}
			}
		})
	}
	wg.Go(func() {
		for range 5 {
			if err := rr.Ping(t.Context()); err != nil {
				t.Errorf("Ping: %v", err)
				return
			}
		}
	})
	wg.Wait()
}

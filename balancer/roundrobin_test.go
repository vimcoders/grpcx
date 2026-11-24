package balancer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
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
// DialContext, so a test can inject its own resolver and dialer. It has no
// cancel function and no keepalive goroutine, so Close on it only skips the
// cleanup: use buildTestRoundRobin when the cleanup path matters.
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

// buildTestRoundRobin builds through rrBuilder, so the picker owns a cancel
// function and the keepalive goroutine that performs the final cleanup after
// Close.
func buildTestRoundRobin(t *testing.T, addrs []resolver.Address, dial func(context.Context) (transport.RoundTripper, error)) *RoundRobin {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	b := rrBuilder{
		cancel:  cancel,
		dial:    dial,
		resolve: func(context.Context) ([]resolver.Address, error) { return addrs, nil },
	}
	picker, err := b.Build(ctx, "example.com:50051")
	if err != nil {
		cancel()
		t.Fatalf("Build: %v", err)
	}
	rr := picker.(*RoundRobin)
	t.Cleanup(func() { _ = rr.Close() })
	return rr
}

func statusCodeOf(err error) codes.Code {
	return status.Code(err)
}

// waitClosed blocks until every round tripper reports at least one Close. The
// cleanup runs in the keepalive goroutine after Close cancels the context, so a
// test cannot assume it has happened by the time Close returns.
func waitClosed(t *testing.T, rts ...*fakeRT) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		closed := 0
		for _, rt := range rts {
			if rt.closeCount() > 0 {
				closed++
			}
		}
		if closed == len(rts) {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("%d of %d backends closed within 2s of Close", closed, len(rts))
		}
		time.Sleep(time.Millisecond)
	}
}

// listenLoopback starts a TCP listener on 127.0.0.1 and returns it with its
// address. An IP literal resolves to exactly one address, so the pool size this
// test asserts does not depend on the host's DNS answers.
func listenLoopback(t *testing.T) (net.Listener, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen on loopback: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	return ln, ln.Addr().String()
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

// TestRoundRobinPickWithoutBackends pins the error for an empty pool: no backend
// survived the dial, so the caller gets a retryable code instead of a nil round
// tripper.
func TestRoundRobinPickWithoutBackends(t *testing.T) {
	rr := newTestRoundRobin(nil, podAddrs(2), nil)
	rt, err := rr.Pick(t.Context(), PickInfo{})
	if statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty pool = %v, want ResourceExhausted", err)
	}
	if rt != nil {
		t.Fatalf("Pick on an empty pool = %v, want nil", rt)
	}
}

// TestBuildCleanupRunsAfterClose pins the cleanup design: Close only cancels the
// context, and the keepalive goroutine closes every backend once the loop has
// returned.
func TestBuildCleanupRunsAfterClose(t *testing.T) {
	first, second := &fakeRT{name: "first"}, &fakeRT{name: "second"}
	var dials atomic.Int32
	rr := buildTestRoundRobin(t, podAddrs(2), func(context.Context) (transport.RoundTripper, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitClosed(t, first, second)
	for name, rt := range map[string]*fakeRT{"first": first, "second": second} {
		if got := rt.closeCount(); got != 1 {
			t.Fatalf("%s backend closed %d times, want 1", name, got)
		}
	}
}

// TestBuildCloseIsRepeatable pins that a second Close is a no-op: the backend
// set is closed once, not once per call.
func TestBuildCloseIsRepeatable(t *testing.T) {
	first, second := &fakeRT{name: "first"}, &fakeRT{name: "second"}
	var dials atomic.Int32
	rr := buildTestRoundRobin(t, podAddrs(2), func(context.Context) (transport.RoundTripper, error) {
		if dials.Add(1) == 1 {
			return first, nil
		}
		return second, nil
	})
	for i := range 2 {
		if err := rr.Close(); err != nil {
			t.Fatalf("Close #%d: %v", i+1, err)
		}
	}
	waitClosed(t, first, second)
	for name, rt := range map[string]*fakeRT{"first": first, "second": second} {
		if got := rt.closeCount(); got != 1 {
			t.Fatalf("%s backend closed %d times after two Close calls, want 1", name, got)
		}
	}
}

// TestRoundRobinKeepaliveStopsOnCancel pins that the keepalive loop ends with
// the context and reports Canceled, which is what lets Close stop the loop. The
// loop must not ping after the cancel.
func TestRoundRobinKeepaliveStopsOnCancel(t *testing.T) {
	rt := &fakeRT{name: "a"}
	rr := newTestRoundRobin([]transport.RoundTripper{rt}, podAddrs(1), nil)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx, transport.WithMaxStreams(16)) }()
	select {
	case err := <-done:
		if statusCodeOf(err) != codes.Canceled {
			t.Fatalf("Keepalive after cancel = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Keepalive did not return within 1s of cancel")
	}
	time.Sleep(20 * time.Millisecond)
	if got := rt.pingCount(); got != 0 {
		t.Fatalf("Keepalive pinged %d times after cancel, want 0", got)
	}
}

// TestRoundRobinPingDropsUnresponsiveBackend pins the health pass: a backend
// that fails its ping is closed and replaced up to the address count.
func TestRoundRobinPingDropsUnresponsiveBackend(t *testing.T) {
	dead := &fakeRT{name: "dead", pingErr: status.Unavailable.Err()}
	rr := newTestRoundRobin(
		[]transport.RoundTripper{dead},
		podAddrs(1),
		func(context.Context) (transport.RoundTripper, error) { return &fakeRT{name: "replacement"}, nil },
	)
	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := dead.closeCount(); got != 1 {
		t.Fatalf("unresponsive backend closed %d times, want 1", got)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool size after a failed ping = %d, want 1", got)
	}
}

// TestRoundRobinPingClosesExcessBackends pins the shrink path: when the endpoint
// resolves to fewer addresses than the pool holds, the surplus backends are
// closed instead of kept alive.
func TestRoundRobinPingClosesExcessBackends(t *testing.T) {
	first, second := &fakeRT{name: "first"}, &fakeRT{name: "second"}
	rr := newTestRoundRobin([]transport.RoundTripper{first, second}, podAddrs(1), nil)
	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := second.closeCount(); got != 1 {
		t.Fatalf("excess backend closed %d times, want 1", got)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool size after shrinking = %d, want 1", got)
	}
}

// TestRoundRobinPingRedialsToAddressCount pins the grow path: an empty pool is
// filled with one connection per resolved address.
func TestRoundRobinPingRedialsToAddressCount(t *testing.T) {
	var dials atomic.Int32
	rr := newTestRoundRobin(nil, podAddrs(2), func(context.Context) (transport.RoundTripper, error) {
		dials.Add(1)
		return &fakeRT{name: "fresh"}, nil
	})
	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("Ping dialed %d backends, want 2", got)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool size after refilling = %d, want 2", got)
	}
}

// TestRoundRobinPingReportsResolveError pins that a failing resolver reaches the
// caller and leaves the pool alone.
func TestRoundRobinPingReportsResolveError(t *testing.T) {
	boom := errors.New("resolver: no addresses found")
	rt := &fakeRT{name: "a"}
	rr := newTestRoundRobin([]transport.RoundTripper{rt}, nil, nil)
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return nil, boom }
	if err := rr.Ping(t.Context()); !errors.Is(err, boom) {
		t.Fatalf("Ping = %v, want %v", err, boom)
	}
	if got := rt.closeCount(); got != 0 {
		t.Fatalf("Ping closed %d backends on a resolve error, want 0", got)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool size after a resolve error = %d, want 1", got)
	}
}

// TestRoundRobinPingSkipsUndialableReplacement pins that a failing dial is
// skipped instead of failing the whole health pass: the surviving backends stay
// and the pool may come out short.
func TestRoundRobinPingSkipsUndialableReplacement(t *testing.T) {
	dead := &fakeRT{name: "dead", pingErr: status.Unavailable.Err()}
	rr := newTestRoundRobin(
		[]transport.RoundTripper{dead},
		podAddrs(1),
		func(context.Context) (transport.RoundTripper, error) {
			return nil, errors.New("dial failed")
		},
	)
	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping with an undialable replacement = %v, want nil", err)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool size after a failed replacement = %d, want 0", got)
	}
}

// TestRoundRobinPingWithEmptyResolutionClosesBackends pins that a resolution
// that comes back empty tears the pool down rather than leaving it untouched.
func TestRoundRobinPingWithEmptyResolutionClosesBackends(t *testing.T) {
	rt := &fakeRT{name: "a"}
	rr := newTestRoundRobin([]transport.RoundTripper{rt}, nil, nil)
	if err := rr.Ping(t.Context()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := rt.closeCount(); got != 1 {
		t.Fatalf("backend closed %d times on an empty resolution, want 1", got)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool size on an empty resolution = %d, want 0", got)
	}
}

// TestBuildToleratesEmptyResolution pins the K8s startup case: an endpoint that
// currently resolves to zero pods still yields a usable picker, every pick fails
// as ResourceExhausted, and the next keepalive pass sizes the pool once pods
// appear. Failing the dial instead would turn a rolling deploy into a crash loop.
func TestBuildToleratesEmptyResolution(t *testing.T) {
	var (
		mu    sync.Mutex
		addrs []resolver.Address // starts empty: no pod is known yet
		dials atomic.Int32
	)
	ctx, cancel := context.WithCancel(t.Context())
	b := rrBuilder{
		cancel: cancel,
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials.Add(1)
			return &fakeRT{}, nil
		},
		resolve: func(context.Context) ([]resolver.Address, error) {
			mu.Lock()
			defer mu.Unlock()
			return addrs, nil
		},
	}

	picker, err := b.Build(ctx, "example.com:50051")
	if err != nil {
		cancel()
		t.Fatalf("Build on a zero-pod resolution = %v, want a working picker", err)
	}
	rr, ok := picker.(*RoundRobin)
	if !ok {
		cancel()
		t.Fatalf("Build returned %T, want *RoundRobin", picker)
	}
	t.Cleanup(func() {
		_ = rr.Close()
		cancel()
	})

	if _, err := rr.Pick(t.Context(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty pool = %v, want ResourceExhausted", err)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("dialed %d backends on a zero-pod resolution, want 0", got)
	}

	// Pods come up: the next keepalive pass has to size the pool to them.
	mu.Lock()
	addrs = podAddrs(2)
	mu.Unlock()
	if err := rr.Ping(ctx); err != nil {
		t.Fatalf("Ping after pods appeared = %v, want nil", err)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialed %d backends after 2 pods appeared, want 2", got)
	}
	if rt, err := rr.Pick(t.Context(), PickInfo{}); err != nil || rt == nil {
		t.Fatalf("Pick after pods appeared = %v, %v, want a round tripper", rt, err)
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
	var dials atomic.Int32
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials.Add(1)
			return &fakeRT{name: fmt.Sprintf("rt%d", dials.Load())}, nil
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return podAddrs(3), nil },
	}
	picker, err := b.Build(t.Context(), "example.com:50051")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer func() { _ = picker.Close() }()
	if got := dials.Load(); got != 3 {
		t.Fatalf("dialed %d backends, want 3", got)
	}
	if _, err := picker.Pick(t.Context(), PickInfo{}); err != nil {
		t.Fatalf("Pick after Build: %v", err)
	}
}

// TestBuildSkipsUndialableAddress pins the per-pod tolerance: a pod that
// refuses the connection is skipped, the rest of the pool still comes up and
// Build reports no error.
func TestBuildSkipsUndialableAddress(t *testing.T) {
	var dials atomic.Int32
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			if dials.Add(1) == 2 {
				return nil, errors.New("dial failed")
			}
			return &fakeRT{name: "up"}, nil
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return podAddrs(3), nil },
	}
	picker, err := b.Build(t.Context(), "example.com:50051")
	if err != nil {
		t.Fatalf("Build with one undialable pod = %v, want nil", err)
	}
	rr := picker.(*RoundRobin)
	defer func() { _ = rr.Close() }()
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool size with one undialable pod = %d, want 2", got)
	}
}

// TestBuildAllDialsFailReturnsEmptyPool pins the current tolerance floor: when
// every pod refuses, Build still returns a picker and reports no error, so the
// failure shows up on the first Pick instead of at dial time.
func TestBuildAllDialsFailReturnsEmptyPool(t *testing.T) {
	b := rrBuilder{
		dial: func(context.Context) (transport.RoundTripper, error) {
			return nil, errors.New("dial failed")
		},
		resolve: func(context.Context) ([]resolver.Address, error) { return podAddrs(2), nil },
	}
	picker, err := b.Build(t.Context(), "example.com:50051")
	if err != nil {
		t.Fatalf("Build with every pod down = %v, want nil", err)
	}
	rr := picker.(*RoundRobin)
	defer func() { _ = rr.Close() }()
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool size with every pod down = %d, want 0", got)
	}
	if _, err := rr.Pick(t.Context(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on a pool where every pod is down = %v, want ResourceExhausted", err)
	}
}

// TestRoundRobinKeepalivePingsOnTick pins the ping cadence: after one keepalive
// period the loop pings the pool, and a ping that fails only drops and replaces
// its backend instead of stopping the loop. The period is fixed at three call
// timeouts (9s) and is not tunable through an Option, so this test is skipped
// under -short.
func TestRoundRobinKeepalivePingsOnTick(t *testing.T) {
	if testing.Short() {
		t.Skip("keepalive period is 9s, too slow for -short")
	}
	dead := &fakeRT{name: "dead", pingErr: status.Unavailable.Err()}
	rr := newTestRoundRobin(
		[]transport.RoundTripper{dead},
		podAddrs(1),
		func(context.Context) (transport.RoundTripper, error) { return &fakeRT{name: "replacement"}, nil },
	)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx, transport.WithMaxStreams(16)) }()
	deadline := time.Now().Add(15 * time.Second)
	for dead.pingCount() == 0 {
		if time.Now().After(deadline) {
			cancel()
			t.Fatal("Keepalive did not ping within 15s")
		}
		time.Sleep(50 * time.Millisecond)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool size after a failed ping = %d, want 1", got)
	}
	if got := dead.closeCount(); got != 1 {
		t.Fatalf("unresponsive backend closed %d times, want 1", got)
	}
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

// TestDialContextBuildsBalancerForReachableEndpoint pins the wiring DialContext
// owns: the real DNS resolver turns an IP literal into one address, the real
// dial opens one TCP connection, and the returned picker serves picks over it.
func TestDialContextBuildsBalancerForReachableEndpoint(t *testing.T) {
	ln, addr := listenLoopback(t)
	accepted := make(chan struct{})
	go func() {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		close(accepted)
		defer func() { _ = conn.Close() }()
		// Drain until the client closes, so its receive loop exits cleanly
		// instead of tripping over a reset connection.
		_, _ = io.Copy(io.Discard, conn)
	}()
	picker, err := DialContext(t.Context(), addr)
	if err != nil {
		t.Fatalf("DialContext(%q): %v", addr, err)
	}
	defer func() { _ = picker.Close() }()
	select {
	case <-accepted:
	case <-time.After(time.Second):
		t.Fatal("DialContext did not open a connection within 1s")
	}
	rt, err := picker.Pick(t.Context(), PickInfo{})
	if err != nil {
		t.Fatalf("Pick after DialContext: %v", err)
	}
	if rt == nil {
		t.Fatal("Pick after DialContext = nil round tripper")
	}
}

// TestDialContextReportsResolveError pins the resolve branch: an empty endpoint
// fails in the DNS closure before any dial, and the error reaches the caller.
func TestDialContextReportsResolveError(t *testing.T) {
	picker, err := DialContext(t.Context(), "")
	if err == nil {
		if picker != nil {
			_ = picker.Close()
		}
		t.Fatal("DialContext on an empty endpoint = nil error, want a resolve error")
	}
	if picker != nil {
		t.Fatalf("DialContext on an empty endpoint = %v, want nil picker", picker)
	}
}

// TestDialContextSurvivesDialError pins the tolerant dial branch: an endpoint
// that resolves but refuses the connection still yields a picker, and the
// failure surfaces on the first Pick.
func TestDialContextSurvivesDialError(t *testing.T) {
	ln, addr := listenLoopback(t)
	if err := ln.Close(); err != nil {
		t.Fatalf("close listener: %v", err)
	}
	picker, err := DialContext(t.Context(), addr)
	if err != nil {
		t.Fatalf("DialContext(%q) with nothing listening = %v, want nil", addr, err)
	}
	defer func() { _ = picker.Close() }()
	if _, err := picker.Pick(t.Context(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick after a refused dial = %v, want ResourceExhausted", err)
	}
}

// TestRoundRobinConcurrentPickAndClose pins that picks and Close race safely: the
// backends are closed in the keepalive goroutine, and picks read the pool under
// the read lock.
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
	t.Logf("closed backends: %d %d %d", a.closeCount(), b.closeCount(), c.closeCount())
}

// TestRoundRobinConcurrentPingAndPick pins that a ping swapping the pool races
// picks safely: the table is swapped under the write lock, and readers either
// see the old or the new snapshot.
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

// TestBuildConcurrentPingAndClose pins that a caller pinging while Close stops
// the keepalive loop does not corrupt the pool or crash the cleanup.
func TestBuildConcurrentPingAndClose(t *testing.T) {
	rr := buildTestRoundRobin(t, podAddrs(1), func(context.Context) (transport.RoundTripper, error) {
		return &fakeRT{name: "fresh"}, nil
	})
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 20 {
				_ = rr.Ping(context.Background())
			}
		})
	}
	wg.Go(func() { _ = rr.Close() })
	wg.Wait()
}

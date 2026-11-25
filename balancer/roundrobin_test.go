package balancer

import (
	"context"
	"errors"
	"fmt"
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

// recordingDialer is a dial function that hands out fakeRTs and remembers every
// one it handed out, so a test can inspect the connections the balancer owns.
// It is safe to call from the keepalive goroutine and from a test at once.
type recordingDialer struct {
	mu    sync.Mutex
	rts   []*fakeRT
	dials atomic.Int32
}

func (d *recordingDialer) dial(context.Context) (transport.RoundTripper, error) {
	d.dials.Add(1)
	d.mu.Lock()
	defer d.mu.Unlock()
	rt := &fakeRT{name: fmt.Sprintf("pod-%d", len(d.rts))}
	d.rts = append(d.rts, rt)
	return rt, nil
}

func (d *recordingDialer) snapshot() []*fakeRT {
	d.mu.Lock()
	defer d.mu.Unlock()
	return append([]*fakeRT{}, d.rts...)
}

func (d *recordingDialer) dialed() int { return int(d.dials.Load()) }

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

// fixedAddresses is the resolver most tests want: a fixed number of addresses.
func fixedAddresses(n int) func(context.Context) ([]resolver.Address, error) {
	return func(context.Context) ([]resolver.Address, error) { return podAddrs(n), nil }
}

// newTestRoundRobin builds a RoundRobin without the DNS/dial closures of
// DialContext, so a test can inject its own resolver and dialer. It carries the
// same defaultLimit Build installs. It has no cancel function and no keepalive
// goroutine, so Close on it only skips the cleanup: use buildTestRoundRobin when
// the cleanup path matters.
func newTestRoundRobin(rts []transport.RoundTripper, resolve func(context.Context) ([]resolver.Address, error), dial func(context.Context) (transport.RoundTripper, error)) *RoundRobin {
	if resolve == nil {
		resolve = fixedAddresses(0)
	}
	if dial == nil {
		dial = func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil }
	}
	return &RoundRobin{
		rts:     rts,
		limit:   defaultLimit,
		resolve: resolve,
		dial:    dial,
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
	waitFor(t, fmt.Sprintf("%d backends to close", len(rts)), func() bool {
		for _, rt := range rts {
			if rt.closeCount() == 0 {
				return false
			}
		}
		return true
	})
}

// waitLimit blocks until Limit reports want. The keepalive goroutine zeroes it
// on the way out, so a test that asserts the settled state has to wait for it.
func waitLimit(t *testing.T, rr *RoundRobin, want int) {
	t.Helper()
	waitFor(t, fmt.Sprintf("limit to become %d", want), func() bool { return rr.Limit() == want })
}

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		if cond() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out after 2s waiting for %s", what)
		}
		time.Sleep(time.Millisecond)
	}
}

func TestBuildDialsOneBackendPerAddress(t *testing.T) {
	d := &recordingDialer{}
	rr := buildTestRoundRobin(t, podAddrs(3), d.dial)

	if got := len(rr.clone()); got != 3 {
		t.Fatalf("pool = %d backends, want 3", got)
	}
	if got := d.dialed(); got != 3 {
		t.Fatalf("dialed %d times, want 3", got)
	}
	if got := rr.Limit(); got != defaultLimit {
		t.Fatalf("Limit = %d, want %d", got, defaultLimit)
	}
}

func TestBuildReportsResolveError(t *testing.T) {
	var dials atomic.Int32
	want := errors.New("dns down")
	ctx, cancel := context.WithCancel(t.Context())
	b := rrBuilder{
		cancel: cancel,
		resolve: func(context.Context) ([]resolver.Address, error) {
			return nil, want
		},
		dial: func(context.Context) (transport.RoundTripper, error) {
			dials.Add(1)
			return &fakeRT{}, nil
		},
	}
	picker, err := b.Build(ctx, "example.com:50051")
	cancel()
	if picker != nil {
		t.Fatalf("picker = %v, want nil on resolve error", picker)
	}
	if !errors.Is(err, want) {
		t.Fatalf("Build error = %v, want %v", err, want)
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("dialed %d times after a resolve error, want 0", got)
	}
}

func TestBuildAllDialsFailReturnsEmptyPool(t *testing.T) {
	rr := buildTestRoundRobin(t, podAddrs(3), func(context.Context) (transport.RoundTripper, error) {
		return nil, errors.New("connection refused")
	})
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool = %d backends, want 0", got)
	}
	if _, err := rr.Pick(context.Background(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty pool = %v, want ResourceExhausted", err)
	}
}

func TestBuildSkipsUndialableAddress(t *testing.T) {
	var calls atomic.Int32
	rr := buildTestRoundRobin(t, podAddrs(3), func(context.Context) (transport.RoundTripper, error) {
		if calls.Add(1) != 2 {
			return nil, errors.New("connection refused")
		}
		return &fakeRT{name: "only"}, nil
	})
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends, want 1", got)
	}
}

func TestBuildAcceptsEmptyResolution(t *testing.T) {
	rr := buildTestRoundRobin(t, nil, func(context.Context) (transport.RoundTripper, error) {
		return &fakeRT{}, nil
	})
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool = %d backends, want 0", got)
	}
	if got := rr.Limit(); got != defaultLimit {
		t.Fatalf("Limit = %d, want %d", got, defaultLimit)
	}
}

func TestPingRedialsToAddressCount(t *testing.T) {
	d := &recordingDialer{}
	rr := newTestRoundRobin(nil, fixedAddresses(3), d.dial)
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 3 {
		t.Fatalf("pool = %d backends, want 3", got)
	}
	if got := d.dialed(); got != 3 {
		t.Fatalf("dialed %d times, want 3", got)
	}
}

func TestPingDropsUnresponsiveBackend(t *testing.T) {
	healthy := &fakeRT{name: "healthy"}
	dead := &fakeRT{name: "dead", pingErr: status.Unavailable.Err()}
	d := &recordingDialer{}
	rr := newTestRoundRobin([]transport.RoundTripper{healthy, dead}, fixedAddresses(2), d.dial)
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := dead.closeCount(); got != 1 {
		t.Fatalf("dead backend closed %d times, want 1", got)
	}
	if got := healthy.closeCount(); got != 0 {
		t.Fatalf("healthy backend closed %d times, want 0", got)
	}
	if got := healthy.pingCount(); got != 1 {
		t.Fatalf("healthy backend pinged %d times, want 1", got)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool = %d backends, want 2", got)
	}
	if got := d.dialed(); got != 1 {
		t.Fatalf("dialed %d replacements, want 1", got)
	}
}

func TestPingClosesExcessBackends(t *testing.T) {
	d := &recordingDialer{}
	var addrs atomic.Int32
	addrs.Store(2)
	rr := newTestRoundRobin(nil, func(context.Context) ([]resolver.Address, error) {
		return podAddrs(int(addrs.Load())), nil
	}, d.dial)
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool = %d backends, want 2", got)
	}

	// The endpoint now resolves to one address, so one backend is redundant.
	addrs.Store(1)
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends after shrink, want 1", got)
	}
	var closed int
	for _, rt := range d.snapshot() {
		if rt.closeCount() > 0 {
			closed++
		}
	}
	if closed != 1 {
		t.Fatalf("%d backends closed after shrink, want 1", closed)
	}
	if got := d.dialed(); got != 2 {
		t.Fatalf("dialed %d times after shrink, want 2 (no redial)", got)
	}
}

func TestPingReportsResolveError(t *testing.T) {
	first := &fakeRT{name: "first"}
	want := errors.New("dns down")
	rr := newTestRoundRobin([]transport.RoundTripper{first}, func(context.Context) ([]resolver.Address, error) {
		return nil, want
	}, nil)
	if err := rr.Ping(context.Background()); !errors.Is(err, want) {
		t.Fatalf("Ping error = %v, want %v", err, want)
	}
	if got := first.closeCount(); got != 0 {
		t.Fatalf("backend closed %d times after a resolve error, want 0", got)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends after a resolve error, want 1 (untouched)", got)
	}
}

func TestPingSkipsUndialableReplacement(t *testing.T) {
	var calls atomic.Int32
	rr := newTestRoundRobin(nil, fixedAddresses(2), func(context.Context) (transport.RoundTripper, error) {
		if calls.Add(1) == 1 {
			return nil, errors.New("connection refused")
		}
		return &fakeRT{name: "late"}, nil
	})
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends, want 1 (one dial failed)", got)
	}
}

func TestPingClosesBackendsWhenResolutionEmpties(t *testing.T) {
	first, second := &fakeRT{name: "first"}, &fakeRT{name: "second"}
	rr := newTestRoundRobin([]transport.RoundTripper{first, second}, fixedAddresses(0), nil)
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool = %d backends after an empty resolution, want 0", got)
	}
	waitClosed(t, first, second)
}

// TestPingCapsPoolAtLimit pins the second condition on both loops: the pool
// never grows past the limit, even when the endpoint resolves to more pods than
// that. Build always installs defaultLimit, so the limit is set directly here to
// keep the bound observable.
func TestPingCapsPoolAtLimit(t *testing.T) {
	var dials atomic.Int32
	rr := newTestRoundRobin(nil, fixedAddresses(4), func(context.Context) (transport.RoundTripper, error) {
		dials.Add(1)
		return &fakeRT{}, nil
	})
	rr.limit = 2
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Fatalf("pool = %d backends, want 2 (the limit)", got)
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialed %d times, want 2 (the limit)", got)
	}

	// The limit also caps a pool that is already over it: one backend is closed
	// and none is dialed.
	rr.limit = 1
	before := dials.Load()
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends after a lower limit, want 1", got)
	}
	var closedAfter int
	for _, rt := range rr.clone() {
		if f, ok := rt.(*fakeRT); ok && f.closeCount() > 0 {
			closedAfter++
		}
	}
	if closedAfter != 0 {
		t.Fatalf("%d pooled backends are closed, want none", closedAfter)
	}
	if got := dials.Load(); got != before {
		t.Fatalf("dialed %d times while shrinking, want 0", got-before)
	}
}

// TestPingWithZeroLimitDrainsPool pins what a zero limit means outside the
// cleanup path: no backend fits, so every one is closed and none is dialed. It
// must not resolve into a pool that holds anything, nil entries included.
func TestPingWithZeroLimitDrainsPool(t *testing.T) {
	first, second := &fakeRT{name: "first"}, &fakeRT{name: "second"}
	var dials atomic.Int32
	rr := newTestRoundRobin([]transport.RoundTripper{first, second}, fixedAddresses(2), func(context.Context) (transport.RoundTripper, error) {
		dials.Add(1)
		return &fakeRT{}, nil
	})
	rr.limit = 0
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping with a zero limit: %v", err)
	}
	if got := len(rr.clone()); got != 0 {
		t.Fatalf("pool = %d entries after a zero limit, want 0", got)
	}
	for i, rt := range rr.clone() {
		if rt == nil {
			t.Fatalf("pool[%d] = nil, want an empty pool", i)
		}
	}
	if got := dials.Load(); got != 0 {
		t.Fatalf("dialed %d times with a zero limit, want 0", got)
	}
}

func TestPickIsRoundRobin(t *testing.T) {
	first, second, third := &fakeRT{name: "a"}, &fakeRT{name: "b"}, &fakeRT{name: "c"}
	rr := newTestRoundRobin([]transport.RoundTripper{first, second, third}, fixedAddresses(3), nil)
	want := []*fakeRT{second, third, first, second}
	for i, w := range want {
		rt, err := rr.Pick(context.Background(), PickInfo{})
		if err != nil {
			t.Fatalf("pick %d: %v", i, err)
		}
		if rt != w {
			t.Fatalf("pick %d = %v, want %v", i, rt.(*fakeRT).name, w.name)
		}
	}
}

func TestPickEmptyPool(t *testing.T) {
	rr := newTestRoundRobin(nil, fixedAddresses(0), nil)
	if _, err := rr.Pick(context.Background(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty pool = %v, want ResourceExhausted", err)
	}
}

// TestPickNeverReturnsNil pins the Picker contract after Close: Pick answers
// with a backend or an error, never with a nil backend and a nil error, because
// every caller hands the result straight to Invoke.
func TestPickNeverReturnsNil(t *testing.T) {
	d := &recordingDialer{}
	rr := buildTestRoundRobin(t, podAddrs(2), d.dial)
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitClosed(t, d.snapshot()...)
	waitLimit(t, rr, 0)

	rt, err := rr.Pick(context.Background(), PickInfo{})
	if rt == nil && err == nil {
		t.Fatal("Pick after Close = (nil, nil), want an error")
	}
	if rt != nil && err != nil {
		t.Fatalf("Pick after Close = (%v, %v), want exactly one of them", rt, err)
	}
}

func TestPickAndPingAreConcurrent(t *testing.T) {
	d := &recordingDialer{}
	rr := newTestRoundRobin(nil, fixedAddresses(2), d.dial)
	var wg sync.WaitGroup
	for range 4 {
		wg.Go(func() {
			for range 50 {
				rt, err := rr.Pick(context.Background(), PickInfo{})
				if err == nil && rt == nil {
					t.Error("Pick returned a nil round tripper with a nil error")
				}
				if err != nil && rt != nil {
					t.Errorf("Pick returned %v together with a round tripper", err)
				}
			}
		})
	}
	wg.Go(func() {
		for range 20 {
			_ = rr.Ping(context.Background())
		}
	})
	wg.Wait()
}

func TestKeepaliveStopsOnCancel(t *testing.T) {
	rr := newTestRoundRobin(nil, fixedAddresses(1), nil)
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx) }()
	cancel()
	select {
	case err := <-done:
		if statusCodeOf(err) != codes.Canceled {
			t.Fatalf("Keepalive = %v, want Canceled", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Keepalive did not return within 2s of cancel")
	}
}

// TestCloseSweepsPool pins the cleanup contract: every backend is closed, the
// limit is zeroed, and a Ping that arrives afterwards opens no new connection.
// Ping runs behind a recover because a nil entry in the pool would panic the
// binary instead of failing this one test.
func TestCloseSweepsPool(t *testing.T) {
	d := &recordingDialer{}
	rr := buildTestRoundRobin(t, podAddrs(2), d.dial)
	before := d.dialed()
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	waitClosed(t, d.snapshot()...)
	waitLimit(t, rr, 0)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Ping after Close panicked: %v", r)
		}
	}()
	_ = rr.Ping(context.Background())
	if got := d.dialed(); got != before {
		t.Fatalf("dialed %d more times after Close, want 0", got-before)
	}
}

func TestCloseIsRepeatable(t *testing.T) {
	var dials atomic.Int32
	rr := buildTestRoundRobin(t, podAddrs(2), func(context.Context) (transport.RoundTripper, error) {
		dials.Add(1)
		return &fakeRT{}, nil
	})
	for i := range 2 {
		if err := rr.Close(); err != nil {
			t.Fatalf("Close %d: %v", i+1, err)
		}
	}
	if got := dials.Load(); got != 2 {
		t.Fatalf("dialed %d times, want 2", got)
	}
}

// TestConcurrentPingAndClose pins the shutdown race: Ping runs while Close tears
// the pool down, so the last Ping has to leave an empty pool behind and open no
// new connection. A backend that a racing Ping stored after the cleanup is closed
// by that final Ping, which is exactly the limit=0 contract.
func TestConcurrentPingAndClose(t *testing.T) {
	d := &recordingDialer{}
	rr := buildTestRoundRobin(t, podAddrs(3), d.dial)
	var wg sync.WaitGroup
	for range 3 {
		wg.Go(func() {
			for range 30 {
				_ = rr.Ping(context.Background())
			}
		})
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	wg.Wait()
	waitLimit(t, rr, 0)

	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Ping after Close panicked: %v", r)
		}
	}()
	before := d.dialed()
	_ = rr.Ping(context.Background())
	if got := d.dialed(); got != before {
		t.Fatalf("dialed %d more times after Close, want 0", got-before)
	}
	pool := rr.clone()
	if len(pool) != 0 {
		t.Fatalf("pool = %d entries after Close, want 0", len(pool))
	}
}

// TestDialContextPoolsOneBackend pins the real entry point: an IP endpoint
// resolves to a single address, so the pool sizes to one connection. Nothing
// accepts on the listener, which is enough: the backend only has to be dialed,
// not handshaked.
func TestDialContextPoolsOneBackend(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	picker, err := DialContext(ctx, ln.Addr().String())
	if err != nil {
		t.Fatalf("DialContext: %v", err)
	}
	defer func() { _ = picker.Close() }()

	rr, ok := picker.(*RoundRobin)
	if !ok {
		t.Fatalf("DialContext returned %T, want *RoundRobin", picker)
	}
	if got := len(rr.clone()); got != 1 {
		t.Fatalf("pool = %d backends, want 1", got)
	}
	if got := rr.Limit(); got != defaultLimit {
		t.Fatalf("Limit = %d, want %d", got, defaultLimit)
	}
}

// TestDialContextToleratesUnreachableEndpoint pins the failure contract: a
// refusal during Build is not an error, it leaves an empty pool that Pick
// reports as ResourceExhausted. A closed port refuses immediately, so the test
// costs no timeout.
func TestDialContextToleratesUnreachableEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	if cerr := ln.Close(); cerr != nil {
		t.Fatalf("close listener: %v", cerr)
	}

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	picker, err := DialContext(ctx, addr)
	if err != nil {
		t.Fatalf("DialContext on a refused endpoint: %v", err)
	}
	defer func() { _ = picker.Close() }()

	if _, err := picker.Pick(context.Background(), PickInfo{}); statusCodeOf(err) != codes.ResourceExhausted {
		t.Fatalf("Pick after a refused dial = %v, want ResourceExhausted", err)
	}
}

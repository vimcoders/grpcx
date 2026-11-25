package balancer

import (
	"context"
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

// fakeRT is one backend connection: it records how often it was pinged and
// closed, and answers calls with Unimplemented because the balancer never sends
// a real request through it in these tests.
type fakeRT struct {
	name      string
	pingErr   error
	pings     atomic.Int32
	closed    atomic.Int32
	dialCount int
}

func (f *fakeRT) RoundTrip(context.Context, *api.Request) (*api.Response, error) {
	return nil, status.Unimplemented.Err()
}

func (f *fakeRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return status.Unimplemented.Err()
}

func (f *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

func (f *fakeRT) Ping(context.Context) error {
	f.pings.Add(1)
	return f.pingErr
}

func (f *fakeRT) Close() error {
	f.closed.Add(1)
	return nil
}

// fakeBackends is the dial hook handed to the builder: it hands out a fresh
// connection per call and remembers the ones it created.
type fakeBackends struct {
	mu      sync.Mutex
	rts     []*fakeRT
	dials   int
	dialErr error
}

func (b *fakeBackends) dial(context.Context) (transport.RoundTripper, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.dials++
	if b.dialErr != nil {
		return nil, b.dialErr
	}
	rt := &fakeRT{name: fmt.Sprintf("rt-%d", b.dials), dialCount: b.dials}
	b.rts = append(b.rts, rt)
	return rt, nil
}

func (b *fakeBackends) created() []*fakeRT {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]*fakeRT{}, b.rts...)
}

// fixedResolve returns n addresses, whatever the context says.
func fixedResolve(n int) func(context.Context) ([]resolver.Address, error) {
	return func(context.Context) ([]resolver.Address, error) {
		addrs := make([]resolver.Address, 0, n)
		for i := range n {
			addrs = append(addrs, resolver.Address{Addr: fmt.Sprintf("10.0.0.%d", i+1)})
		}
		return addrs, nil
	}
}

var _ transport.RoundTripper = (*fakeRT)(nil)

// buildFor creates a balancer over the fake dial hook and the given number of
// resolved addresses, closing it at cleanup.
func buildFor(t *testing.T, backends *fakeBackends, addresses int) *RoundRobin {
	t.Helper()
	b := &rrBuilder{dial: backends.dial, resolve: fixedResolve(addresses)}
	p, err := b.Build(context.Background(), "endpoint")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	rr := p.(*RoundRobin)
	t.Cleanup(func() {
		_ = rr.Close()
		waitFor(t, func() bool { return rr.Limit() == 0 })
	})
	return rr
}

// waitFor polls cond until it holds, failing the test after a second.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition did not hold within a second")
}

// TestBuildDialsOneConnectionPerAddress covers the pool size: the pool is filled
// up to the number of addresses the endpoint resolves to, and the default limit
// is the ceiling it may never exceed.
func TestBuildDialsOneConnectionPerAddress(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 3)

	if got := len(rr.clone()); got != 3 {
		t.Errorf("pool holds %d connections, want 3", got)
	}
	if got := backends.dials; got != 3 {
		t.Errorf("dial called %d times, want 3", got)
	}
	if got := rr.Limit(); got != defaultLimit {
		t.Errorf("Limit() = %d, want the default %d", got, defaultLimit)
	}
}

// TestBuildSucceedsWithAnEmptyPoolWhenEveryDialFails pins the current behaviour
// of Ping: a dial error is skipped, so a balancer whose every dial failed still
// builds, and Pick is what refuses the call with Unavailable. Build only reports
// the resolve error.
func TestBuildSucceedsWithAnEmptyPoolWhenEveryDialFails(t *testing.T) {
	backends := &fakeBackends{dialErr: status.Unavailable.Err()}
	b := &rrBuilder{dial: backends.dial, resolve: fixedResolve(1)}

	p, err := b.Build(context.Background(), "endpoint")
	if err != nil {
		t.Fatalf("Build = %v, want nil while Pick carries the refusal", err)
	}
	rr := p.(*RoundRobin)
	t.Cleanup(func() {
		_ = rr.Close()
		waitFor(t, func() bool { return rr.Limit() == 0 })
	})
	if got := len(rr.clone()); got != 0 {
		t.Errorf("pool holds %d connections, want 0", got)
	}
	if _, err := rr.Pick(context.Background(), PickInfo{}); code(err) != codes.Unavailable {
		t.Errorf("Pick = %v, want Unavailable", err)
	}
}

// TestBuildReportsAResolveFailure covers an endpoint that resolves to nothing:
// the error comes back instead of an empty balancer.
func TestBuildReportsAResolveFailure(t *testing.T) {
	b := &rrBuilder{
		dial:    (&fakeBackends{}).dial,
		resolve: func(context.Context) ([]resolver.Address, error) { return nil, status.Unavailable.Err() },
	}
	if _, err := b.Build(context.Background(), "endpoint"); code(err) != codes.Unavailable {
		t.Errorf("Build = %v, want Unavailable", err)
	}
}

// code is status.Code with a nil guard, so a missing error cannot be mistaken
// for OK.
func code(err error) codes.Code {
	if err == nil {
		return codes.OK
	}
	return status.Code(err)
}

// TestPickRotatesOverThePool covers the distribution: every pick advances one
// step, so the calls spread over the pool in dial order.
func TestPickRotatesOverThePool(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 3)

	want := []string{"rt-2", "rt-3", "rt-1", "rt-2", "rt-3", "rt-1"}
	for i, name := range want {
		rt, err := rr.Pick(context.Background(), PickInfo{})
		if err != nil {
			t.Fatalf("pick #%d: %v", i+1, err)
		}
		if got := rt.(*fakeRT).name; got != name {
			t.Fatalf("pick #%d = %s, want %s", i+1, got, name)
		}
	}
}

// TestPickReportsAnEmptyPool covers a balancer with no backend: the call is
// refused with Unavailable instead of a nil connection.
func TestPickReportsAnEmptyPool(t *testing.T) {
	rr := &RoundRobin{}
	rt, err := rr.Pick(context.Background(), PickInfo{})
	if code(err) != codes.Unavailable {
		t.Errorf("Pick on an empty pool = %v, want Unavailable", err)
	}
	if rt != nil {
		t.Error("Pick returned a connection together with the error")
	}
}

// TestKeepaliveClearsThePoolWhenItEnds covers the shutdown order Build installs:
// once the keepalive loop returns, the pool is emptied and the limit is zero, so
// no later call can dial. The connections are not closed here: the pool drops
// them and each receive loop closes its own connection when the read deadline
// (defaultReadTimeout) fires.
func TestKeepaliveClearsThePoolWhenItEnds(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 3)
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	waitFor(t, func() bool { return rr.Limit() == 0 })
	if got := len(rr.clone()); got != 0 {
		t.Errorf("pool still holds %d connections after shutdown", got)
	}
	for _, rt := range backends.created() {
		if got := rt.closed.Load(); got != 0 {
			t.Errorf("%s was closed %d times, want 0: the read deadline is what closes it", rt.name, got)
		}
	}
	if rt, err := rr.Pick(context.Background(), PickInfo{}); code(err) != codes.Unavailable {
		t.Errorf("Pick after shutdown = (%v, %v), want Unavailable", rt, code(err))
	}
}

// TestPingDropsDeadBackendsAndDialsReplacements covers the health pass: a
// backend that fails its ping is dropped and replaced, so the pool still
// matches the number of pods. The connection itself is left alone — its own
// receive loop closes it when the read deadline fires.
func TestPingDropsDeadBackendsAndDialsReplacements(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 3)
	created := backends.created()
	created[1].pingErr = status.Unavailable.Err()

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 3 {
		t.Errorf("pool holds %d connections, want 3 after the replacement", got)
	}
	if got := created[1].closed.Load(); got != 0 {
		t.Errorf("the dead backend was closed %d times, want 0: the receive loop reclaims it when the read deadline fires", got)
	}
	if got := backends.dials; got != 4 {
		t.Errorf("dial called %d times, want one replacement (4)", got)
	}
}

// TestPingDropsSurplusBackendsWhenTheEndpointShrinks covers pod removal: the
// connections beyond the resolved count leave the pool, but Ping does not close
// them — each receive loop closes its own connection when the read deadline
// (defaultReadTimeout) fires.
func TestPingDropsSurplusBackendsWhenTheEndpointShrinks(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 3)
	rr.resolve = fixedResolve(1)

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 1 {
		t.Errorf("pool holds %d connections, want 1", got)
	}
	kept := rr.clone()[0].(*fakeRT)
	for _, rt := range backends.created() {
		if rt == kept {
			continue
		}
		if got := rt.closed.Load(); got != 0 {
			t.Errorf("%s was closed %d times, want 0: the read deadline is what closes it", rt.name, got)
		}
	}
}

// TestPingNeverExceedsTheLimit covers the pool ceiling: an endpoint that resolves
// to more addresses than the limit admits dials only up to the limit.
func TestPingNeverExceedsTheLimit(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 0)
	rr.limit = 2
	rr.resolve = fixedResolve(5)

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Errorf("pool holds %d connections, want the limit of 2", got)
	}
	if got := backends.dials; got != 2 {
		t.Errorf("dial called %d times, want 2", got)
	}
}

// TestPingKeepsThePoolWhenTheResolveFails covers a transient DNS failure: the
// existing connections are kept, nothing is dropped on a lookup error.
func TestPingKeepsThePoolWhenTheResolveFails(t *testing.T) {
	backends := &fakeBackends{}
	rr := buildFor(t, backends, 2)
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return nil, status.Unavailable.Err() }

	if err := rr.Ping(context.Background()); code(err) != codes.Unavailable {
		t.Fatalf("Ping = %v, want the resolve error", err)
	}
	if got := len(rr.clone()); got != 2 {
		t.Errorf("pool holds %d connections, want the 2 it had", got)
	}
}

// TestKeepaliveReportsCanceledWhenItsContextEnds covers the loop's exit value.
func TestKeepaliveReportsCanceledWhenItsContextEnds(t *testing.T) {
	rr := &RoundRobin{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if err := rr.Keepalive(ctx); code(err) != codes.Canceled {
		t.Errorf("Keepalive = %v, want Canceled", err)
	}
}

// TestCloseToleratesAMissingCancel covers a balancer that was never built:
// Close is still safe, and calling it twice changes nothing.
func TestCloseToleratesAMissingCancel(t *testing.T) {
	rr := &RoundRobin{}
	if err := rr.Close(); err != nil {
		t.Errorf("Close on an unbuilt balancer = %v, want nil", err)
	}
	if err := rr.Close(); err != nil {
		t.Errorf("second Close = %v, want nil", err)
	}
}

// TestDialBuildsAPickerOverAReachableEndpoint covers the real wiring: an
// endpoint the resolver knows dials one connection, and Pick hands it out.
func TestDialBuildsAPickerOverAReachableEndpoint(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer func() { _ = ln.Close() }()

	p, err := Dial(ln.Addr().String())
	if err != nil {
		t.Fatalf("Dial: %v", err)
	}
	rr := p.(*RoundRobin)
	defer func() {
		_ = p.Close()
		waitFor(t, func() bool { return rr.Limit() == 0 })
	}()

	if got := len(rr.clone()); got != 1 {
		t.Errorf("pool holds %d connections, want 1 for a single address", got)
	}
	rt, err := p.Pick(context.Background(), PickInfo{})
	if err != nil {
		t.Fatalf("Pick: %v", err)
	}
	if rt == nil {
		t.Fatal("Pick returned no connection")
	}
}

// TestDialContextReportsAnUnresolvableEndpoint covers the real resolver wiring:
// an endpoint DNS cannot resolve fails the dial.
func TestDialContextReportsAnUnresolvableEndpoint(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	p, err := DialContext(ctx, "no-such-host.invalid:1")
	if err == nil {
		_ = p.Close()
		t.Fatal("DialContext resolved an endpoint that cannot exist")
	}
	if p != nil {
		t.Errorf("DialContext returned a picker together with the error")
	}
}

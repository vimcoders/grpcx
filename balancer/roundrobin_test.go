package balancer

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/resolver"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// fakeRT stands in for one dialed backend: Ping reports what the test
// configured and every other call is a no-op.
type fakeRT struct {
	pingErr error
	pings   int
}

// RoundTrip implements transport.RoundTripper.
func (f *fakeRT) RoundTrip(context.Context, []byte, ...transport.CallOption) (transport.Buffer, error) {
	return nil, nil
}

// Ping implements transport.RoundTripper.
func (f *fakeRT) Ping(context.Context) error {
	f.pings++
	return f.pingErr
}

// Invoke implements grpc.ClientConnInterface.
func (f *fakeRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return nil
}

// NewStream implements grpc.ClientConnInterface.
func (f *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

// Close implements io.Closer.
func (f *fakeRT) Close() error { return nil }

// newRoundRobin builds a balancer over the given backends.
func newRoundRobin(rts ...transport.RoundTripper) *RoundRobin {
	return &RoundRobin{rts: rts, limit: defaultLimit}
}

// resolved returns n addresses, enough for Ping to want n backends.
func resolved(n int) []resolver.Address {
	addrs := make([]resolver.Address, n)
	for i := range addrs {
		addrs[i] = resolver.Address{Addr: "127.0.0.1:1", ServerName: "test"}
	}
	return addrs
}

// TestPickRotates covers the rotation: each pick advances one backend, in order
// and wrapping at the end.
func TestPickRotates(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{}, &fakeRT{}, &fakeRT{}}
	rr := newRoundRobin(rts...)
	for _, want := range []int{1, 2, 0, 1} {
		rt, err := rr.Pick(context.Background(), PickInfo{})
		if err != nil {
			t.Fatalf("Pick: %v", err)
		}
		if rt != rts[want] {
			t.Errorf("picked %p, want rts[%d]", rt, want)
		}
	}
}

// TestPickReportsAnEmptyPool covers the no-backend case: Unavailable, so the
// caller can retry.
func TestPickReportsAnEmptyPool(t *testing.T) {
	rr := newRoundRobin()
	if _, err := rr.Pick(context.Background(), PickInfo{}); status.Code(err) != codes.Unavailable {
		t.Errorf("code = %v, want Unavailable", status.Code(err))
	}
}

// TestLimitReportsThePoolCap covers the field the keepalive loop zeroes on
// exit: it starts at the default.
func TestLimitReportsThePoolCap(t *testing.T) {
	if got := newRoundRobin().Limit(); got != defaultLimit {
		t.Errorf("Limit = %d, want %d", got, defaultLimit)
	}
}

// TestPingKeepsLiveBackends covers the health pass when nothing changed: a live
// backend is kept.
func TestPingKeepsLiveBackends(t *testing.T) {
	live := &fakeRT{}
	rr := newRoundRobin(live)
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(1), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) {
		t.Fatal("dialed although the pool was already full")
		return nil, nil
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if live.pings != 1 {
		t.Errorf("pinged %d times, want 1", live.pings)
	}
	if got := len(rr.rts); got != 1 {
		t.Errorf("pool holds %d backends, want 1", got)
	}
}

// TestPingDropsDeadBackends covers a backend whose ping fails: it leaves the
// pool and nothing replaces it, because the address is gone.
func TestPingDropsDeadBackends(t *testing.T) {
	dead := &fakeRT{pingErr: errors.New("dead")}
	rr := newRoundRobin(dead)
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(1), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) { return nil, errors.New("no dial") }
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.rts); got != 0 {
		t.Errorf("pool holds %d backends, want the dead one dropped", got)
	}
}

// TestPingStopsAtTheResolvedCount covers the surplus rule: more backends than
// the endpoint resolves to are dropped.
func TestPingStopsAtTheResolvedCount(t *testing.T) {
	rr := newRoundRobin(&fakeRT{}, &fakeRT{}, &fakeRT{})
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(2), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil }
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.rts); got != 2 {
		t.Errorf("pool holds %d backends, want 2", got)
	}
}

// TestPingStopsAtTheLimit covers the cap: the resolved count cannot push the
// pool past Limit.
func TestPingStopsAtTheLimit(t *testing.T) {
	rr := newRoundRobin(&fakeRT{}, &fakeRT{}, &fakeRT{})
	rr.limit = 1
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(3), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) {
		t.Fatal("dialed past the limit")
		return nil, nil
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.rts); got != 1 {
		t.Errorf("pool holds %d backends, want 1", got)
	}
}

// TestPingDialsUpToTheResolvedCount covers the growth path: an empty pool is
// filled with one dial per resolved address.
func TestPingDialsUpToTheResolvedCount(t *testing.T) {
	dials := 0
	rr := newRoundRobin()
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(3), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) {
		dials++
		return &fakeRT{}, nil
	}
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if dials != 3 {
		t.Errorf("dialed %d times, want 3", dials)
	}
	if got := len(rr.rts); got != 3 {
		t.Errorf("pool holds %d backends, want 3", got)
	}
}

// TestPingToleratesAFailedDial covers a replacement that cannot connect: the
// pool stays short instead of failing the pass.
func TestPingToleratesAFailedDial(t *testing.T) {
	rr := newRoundRobin()
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return resolved(2), nil }
	rr.dial = func(context.Context) (transport.RoundTripper, error) { return nil, errors.New("boom") }
	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.rts); got != 0 {
		t.Errorf("pool holds %d backends, want none", got)
	}
}

// TestPingReportsAResolveFailure covers the one failure Ping does report: without
// addresses there is nothing to keep or dial.
func TestPingReportsAResolveFailure(t *testing.T) {
	boom := errors.New("dns")
	rr := newRoundRobin()
	rr.resolve = func(context.Context) ([]resolver.Address, error) { return nil, boom }
	if err := rr.Ping(context.Background()); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

// TestCloseCancelsTheKeepaliveLoop covers teardown: Close cancels the balancer
// context, which is what ends the loop.
func TestCloseCancelsTheKeepaliveLoop(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	rr := newRoundRobin()
	rr.cancel = cancel
	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	select {
	case <-ctx.Done():
	case <-time.After(time.Second):
		t.Fatal("Close did not cancel the balancer context")
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestBuildPingsBeforeReturning covers construction: the builder runs one
// health pass before handing the picker out, so a caller never gets a picker
// that has never reached a backend.
func TestBuildPingsBeforeReturning(t *testing.T) {
	b := &rrBuilder{
		resolve: func(context.Context) ([]resolver.Address, error) { return resolved(1), nil },
		dial:    func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil },
	}
	p, err := b.Build(context.Background(), "test")
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	defer p.Close()
	if _, err := p.Pick(context.Background(), PickInfo{}); err != nil {
		t.Errorf("Pick after Build: %v", err)
	}
}

// TestBuildFailsOnTheFirstPing covers an unreachable endpoint at construction:
// the error is returned and the balancer context is canceled.
func TestBuildFailsOnTheFirstPing(t *testing.T) {
	boom := errors.New("no dns")
	b := &rrBuilder{
		resolve: func(context.Context) ([]resolver.Address, error) { return nil, boom },
		dial:    func(context.Context) (transport.RoundTripper, error) { return &fakeRT{}, nil },
	}
	if _, err := b.Build(context.Background(), "test"); !errors.Is(err, boom) {
		t.Errorf("err = %v, want %v", err, boom)
	}
}

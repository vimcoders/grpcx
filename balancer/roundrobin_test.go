package balancer

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
)

// fakeRT is a RoundTripper that counts closes and can fail a ping, which is all
// the balancer reads from a backend.
type fakeRT struct {
	name    string
	mu      sync.Mutex
	closes  int
	pingErr error
}

func (f *fakeRT) RoundTrip(context.Context, *api.Request) (*api.Response, error) {
	return &api.Response{}, nil
}

func (f *fakeRT) Ping(context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.pingErr
}

func (f *fakeRT) Invoke(context.Context, string, any, any, ...grpc.CallOption) error {
	return nil
}

func (f *fakeRT) NewStream(context.Context, *grpc.StreamDesc, string, ...grpc.CallOption) (grpc.ClientStream, error) {
	return nil, status.Unimplemented.Err()
}

func (f *fakeRT) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closes++
	return nil
}

func (f *fakeRT) closeCount() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closes
}

func (f *fakeRT) failPing(err error) {
	f.mu.Lock()
	f.pingErr = err
	f.mu.Unlock()
}

// newRoundRobin builds a balancer over the given backends with injected resolve
// and dial hooks, so no test touches DNS or the network.
func newRoundRobin(rts []transport.RoundTripper, pods int, dial func(context.Context) (transport.RoundTripper, error)) *RoundRobin {
	return &RoundRobin{
		rts:     rts,
		dial:    dial,
		resolve: func(context.Context) (int, error) { return pods, nil },
	}
}

// TestRoundRobinPickRotates pins the rotation: every backend is used in turn,
// and the index wraps back to the first. A picker that kept returning the same
// backend would still look correct with one address and silently pin all traffic
// when several exist.
func TestRoundRobinPickRotates(t *testing.T) {
	a, b, c := &fakeRT{name: "a"}, &fakeRT{name: "b"}, &fakeRT{name: "c"}
	rr := newRoundRobin([]transport.RoundTripper{a, b, c}, 3, nil)

	want := []string{"a", "b", "c", "a"}
	for i, name := range want {
		rt, err := rr.Pick(context.Background(), PickInfo{})
		if err != nil {
			t.Fatalf("Pick %d: %v", i, err)
		}
		if got := rt.(*fakeRT).name; got != name {
			t.Fatalf("Pick %d = %q, want %q", i, got, name)
		}
	}
}

// TestRoundRobinPickWithoutBackends pins the empty table: a balancer with no
// backend reports a status instead of handing back a nil round tripper the
// caller would dereference.
func TestRoundRobinPickWithoutBackends(t *testing.T) {
	rr := newRoundRobin(nil, 0, nil)

	rt, err := rr.Pick(context.Background(), PickInfo{})
	if status.Code(err) != codes.ResourceExhausted {
		t.Fatalf("Pick on an empty balancer = %v, want ResourceExhausted", err)
	}
	if rt != nil {
		t.Fatalf("Pick = %v, want a nil round tripper alongside the error", rt)
	}
}

// TestRoundRobinCloseClosesEveryBackend pins the whole point of Close: canceling
// the balancer context only stops new work, so the connections themselves have
// to be closed here. Leaving them open is what left a benchmark with connections
// that only died on their read deadline.
func TestRoundRobinCloseClosesEveryBackend(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{}, &fakeRT{}, &fakeRT{}}
	rr := newRoundRobin(rts, 3, nil)

	if err := rr.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	for i, rt := range rts {
		if got := rt.(*fakeRT).closeCount(); got != 1 {
			t.Fatalf("backend %d closed %d times, want 1", i, got)
		}
	}
}

// TestRoundRobinCloseIsRepeatable pins that a second Close does not panic on a
// nil cancel function and still reaches the backends. The transport Close
// underneath is a sync.Once, so a repeat close is harmless there.
func TestRoundRobinCloseIsRepeatable(t *testing.T) {
	rt := &fakeRT{}
	rr := newRoundRobin([]transport.RoundTripper{rt}, 1, nil)

	if err := rr.Close(); err != nil {
		t.Fatalf("first Close: %v", err)
	}
	if err := rr.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
	if got := rt.closeCount(); got < 1 {
		t.Fatalf("backend closed %d times, want it closed at least once", got)
	}
}

// TestRoundRobinKeepaliveStopsOnCancel pins that the health loop ends with its
// context: it must not outlive the balancer and keep dialing replacements
// against an endpoint nobody uses any more.
func TestRoundRobinKeepaliveStopsOnCancel(t *testing.T) {
	rr := newRoundRobin(nil, 0, nil)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	done := make(chan error, 1)
	go func() { done <- rr.Keepalive(ctx) }()

	select {
	case err := <-done:
		if status.Code(err) != codes.Canceled {
			t.Fatalf("Keepalive = %v, want Canceled", err)
		}
	case <-time.After(time.Second):
		t.Fatal("Keepalive did not return after its context was canceled")
	}
}

// TestRoundRobinPingDropsUnresponsiveBackend pins the health pass: a backend
// that fails its ping is closed and replaced up to the resolved pod count, which
// is how a churned pod leaves the rotation.
func TestRoundRobinPingDropsUnresponsiveBackend(t *testing.T) {
	alive, dead := &fakeRT{name: "alive"}, &fakeRT{name: "dead"}
	dead.failPing(errors.New("ping refused"))
	replacement := &fakeRT{name: "replacement"}

	dials := 0
	rr := newRoundRobin([]transport.RoundTripper{alive, dead}, 2, func(context.Context) (transport.RoundTripper, error) {
		dials++
		return replacement, nil
	})

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := dead.closeCount(); got != 1 {
		t.Fatalf("dead backend closed %d times, want 1", got)
	}
	if dials != 1 {
		t.Fatalf("dialed %d replacements, want 1", dials)
	}
	if got := len(rr.rts); got != 2 {
		t.Fatalf("backends after Ping = %d, want 2", got)
	}
	if rr.rts[1] != replacement {
		t.Fatalf("replacement not installed at the end of the table")
	}
}

// TestRoundRobinPingClosesExcessBackends pins the shrink path: when the endpoint
// resolves to fewer pods than the table holds, the surplus connections are
// closed instead of being kept around forever.
func TestRoundRobinPingClosesExcessBackends(t *testing.T) {
	first, surplus := &fakeRT{name: "first"}, &fakeRT{name: "surplus"}
	rr := newRoundRobin([]transport.RoundTripper{first, surplus}, 1, nil)

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := surplus.closeCount(); got != 1 {
		t.Fatalf("surplus backend closed %d times, want 1", got)
	}
	if got := len(rr.rts); got != 1 {
		t.Fatalf("backends after Ping = %d, want 1", got)
	}
}

// TestRoundRobinPingRedialsToPodCount pins the grow path: an empty table is
// filled up to the resolved pod count.
func TestRoundRobinPingRedialsToPodCount(t *testing.T) {
	dials := 0
	rr := newRoundRobin(nil, 2, func(context.Context) (transport.RoundTripper, error) {
		dials++
		return &fakeRT{}, nil
	})

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if dials != 2 {
		t.Fatalf("dialed %d backends, want 2", dials)
	}
	if got := len(rr.rts); got != 2 {
		t.Fatalf("backends after Ping = %d, want 2", got)
	}
}

// TestRoundRobinPingReportsResolveError pins that a failing resolution stops the
// pass: the table must keep whatever it has rather than being emptied on a
// transient DNS failure.
func TestRoundRobinPingReportsResolveError(t *testing.T) {
	rt := &fakeRT{}
	rr := &RoundRobin{
		rts: []transport.RoundTripper{rt},
		resolve: func(context.Context) (int, error) {
			return 0, errors.New("resolver: no addresses found")
		},
	}

	if err := rr.Ping(context.Background()); err == nil {
		t.Fatal("Ping with a failing resolver = nil error")
	}
	if got := len(rr.rts); got != 1 {
		t.Fatalf("backends after a failed resolve = %d, want the existing 1 kept", got)
	}
	if got := rt.closeCount(); got != 0 {
		t.Fatalf("backend closed %d times on a failed resolve, want 0", got)
	}
}

// TestRoundRobinPingSkipsUndialableReplacement pins that a dial failure does not
// install a nil backend: the table keeps the backends it has and the next pass
// retries.
func TestRoundRobinPingSkipsUndialableReplacement(t *testing.T) {
	rr := newRoundRobin(nil, 1, func(context.Context) (transport.RoundTripper, error) {
		return nil, errors.New("connection refused")
	})

	if err := rr.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
	if got := len(rr.rts); got != 0 {
		t.Fatalf("backends after a failed dial = %d, want 0", got)
	}
}

// TestRoundRobinConcurrentPickAndClose pins that picking while closing is safe:
// Pick takes the read lock and Close walks the table under the write lock. The
// race detector, which runs in CI, is what checks the memory-model side.
func TestRoundRobinConcurrentPickAndClose(t *testing.T) {
	rts := []transport.RoundTripper{&fakeRT{}, &fakeRT{}}
	rr := newRoundRobin(rts, 2, nil)

	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 16 {
				_, _ = rr.Pick(context.Background(), PickInfo{})
			}
		})
	}
	wg.Go(func() { _ = rr.Close() })
	wg.Wait()
}

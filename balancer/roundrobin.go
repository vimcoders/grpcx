package balancer

import (
	"context"
	"log/slog"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

	"github.com/vimcoders/grpcx/resolver"
)

const (
	// RoundRobinName is the name of round robin balancer.
	RoundRobinName = "round_robin"
	// defaultStep is the default step of round robin balancer.
	defaultStep  = 1
	defaultLimit = 1024
)

// rrBuilder is the builder of round robin balancer.
type rrBuilder struct {
	dial    func(context.Context) (transport.RoundTripper, error)
	resolve func(context.Context) ([]resolver.Address, error)
}

// Build builds a round robin balancer.
func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	childCtx, cancel := context.WithCancel(context.Background())
	// Create a round robin balancer.
	var x = RoundRobin{
		dial:    b.dial,
		resolve: b.resolve,
		cancel:  cancel,
		limit:   defaultLimit,
	}
	go func() {
		// The keepalive loop ends with the balancer's context, so only an
		// unexpected exit is worth reporting.
		if err := x.Keepalive(childCtx, opts...); err != nil {
			slog.Debug("balancer: keepalive exited", "endpoint", endpoint, "error", err)
		}
		x.Lock()
		defer x.Unlock()
		// Emptying the pool is enough: each connection is closed by its own
		// receive loop on the read deadline, so nothing here tears them down.
		x.limit = 0
		x.rts = x.rts[:0]
	}()
	if err := x.Ping(ctx); err != nil {
		cancel()
		return nil, err
	}
	// Return the round robin balancer.
	return &x, nil
}

// RoundRobin is a round robin balancer.
type RoundRobin struct {
	rts     []transport.RoundTripper
	step    atomic.Uint32
	dial    func(context.Context) (transport.RoundTripper, error)
	resolve func(context.Context) ([]resolver.Address, error)
	cancel  context.CancelFunc // Cancelled on close.
	limit   int
	sync.RWMutex
}

// Pick picks a round tripper from the round robin balancer.
func (rr *RoundRobin) Pick(_ context.Context, _ PickInfo) (transport.RoundTripper, error) {
	rr.RLock()
	defer rr.RUnlock()
	rts := rr.rts
	if len(rts) == 0 {
		return nil, status.Unavailable.Err()
	}
	idx := rr.step.Add(defaultStep) % uint32(len(rts))
	return rts[idx], nil
}

// Limit returns the maximum number of backend connections the pool may hold.
// Build installs defaultLimit and the keepalive loop zeroes it when it exits, so
// a zero limit means the pool admits no backend: Ping drops what is left and
// dials nothing.
func (rr *RoundRobin) Limit() int {
	rr.RLock()
	defer rr.RUnlock()
	return rr.limit
}

// DialContext builds a round robin balancer for endpoint: it resolves the
// endpoint to the number of backends, dials one round tripper per backend and
// starts the keepalive loop. That loop runs on a context of its own, so Close —
// not the ctx passed here — ends it; ctx only bounds the resolve and the first
// health pass.
func DialContext(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	b := rrBuilder{
		dial: func(childCtx context.Context) (transport.RoundTripper, error) {
			return transport.DialContext(childCtx, endpoint, opts...)
		},
		resolve: func(childCtx context.Context) ([]resolver.Address, error) {
			resolver := resolver.GetResolver("dns")
			address, err := resolver.Resolve(childCtx, url.URL{Scheme: "dns", Host: endpoint})
			if err != nil {
				return nil, err
			}
			return address, nil
		},
	}
	p, err := b.Build(ctx, endpoint, opts...)
	if err != nil {
		return nil, err
	}
	return p, nil
}

// Dial builds a Picker with no dial deadline, keeping one connection per address
// resolved from endpoint. Pick decides which of those connections carries a call;
// the endpoint's own redundancy is the K8s Service in front of it.
func Dial(endpoint string, opts ...transport.Option) (Picker, error) {
	return DialContext(context.Background(), endpoint, opts...)
}

// Keepalive pings every backend once per keepalive period until ctx is canceled.
// A failed ping is logged at debug level: pods churn, and the ping itself drops
// the dead backend and dials a replacement.
func (rr *RoundRobin) Keepalive(ctx context.Context, opts ...transport.Option) error {
	defaultOptions := transport.DefaultOptions()
	for _, opt := range opts {
		opt(&defaultOptions)
	}
	ticker := time.NewTicker(defaultOptions.Keepalive())
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		case <-ticker.C:
			// A failed ping is expected while pods churn: the connection that
			// answered it is replaced below. Report it at debug level only.
			if err := rr.Ping(ctx); err != nil {
				slog.Debug("balancer: keepalive ping failed", "error", err)
			}
		}
	}
}

// Ping re-resolves the endpoint, drops round trippers that no longer answer,
// dials replacements up to the number of pods the endpoint resolves to, and
// replaces the pool with what survived. Only a failed resolve is reported: a
// backend that fails its ping is dropped from the pool and replaced rather than
// returned as an error, and a failed replacement dial leaves the pool short of
// the resolved count instead of failing the pass. Ping never closes a
// connection: a dropped backend's receive loop closes it when the read deadline
// fires. It is the periodic health pass behind Keepalive.
func (rr *RoundRobin) Ping(ctx context.Context) error {
	ips, err := rr.resolve(ctx)
	if err != nil {
		return err
	}
	limit := rr.Limit()
	var rts []transport.RoundTripper
	// Keep every round tripper that answers and still fits both under the
	// number of resolved addresses and under the limit. A backend whose ping
	// fails, and a surplus one that no longer fits, are both only dropped here:
	// their own receive loop closes the connection when the read deadline fires.
	for _, rt := range rr.clone() {
		if err := rt.Ping(ctx); err != nil {
			continue
		}
		if len(rts) >= len(ips) || len(rts) >= limit {
			continue
		}
		rts = append(rts, rt)
	}
	// Create new round trippers for any new addresses.
	for i := len(rts); i < len(ips) && i < limit; i++ {
		rt, err := rr.dial(ctx)
		if err != nil {
			continue
		}
		rts = append(rts, rt)
	}
	rr.Lock()
	defer rr.Unlock()
	// Update the round tripper list.
	rr.rts = rts
	return nil
}

func (rr *RoundRobin) clone() []transport.RoundTripper {
	rr.RLock()
	defer rr.RUnlock()
	return append([]transport.RoundTripper{}, rr.rts...)
}

// Close stops the balancer by cancelling its context. It does not close the
// backend round trippers: after the keepalive loop returns the pool is emptied
// and the limit is zeroed, and every connection is left to its own receive loop,
// which closes it once its read deadline (defaultReadTimeout) fires. Close is
// safe to call more than once, and returns nil either way.
func (rr *RoundRobin) Close() error {
	if rr.cancel != nil {
		rr.cancel()
	}
	return nil
}

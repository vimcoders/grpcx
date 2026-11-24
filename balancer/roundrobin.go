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
	defaultStep = 1
)

// rrBuilder is the builder of round robin balancer.
type rrBuilder struct {
	dial    func(context.Context) (transport.RoundTripper, error)
	resolve func(context.Context) ([]resolver.Address, error)
	cancel  context.CancelFunc // Cancelled on close.
}

// Build builds a round robin balancer.
func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	// Create a round robin balancer.
	var x = RoundRobin{
		dial:    b.dial,
		resolve: b.resolve,
		cancel:  b.cancel,
	}
	go func() {
		// The keepalive loop ends with the balancer's context, so only an
		// unexpected exit is worth reporting.
		if err := x.Keepalive(ctx, opts...); err != nil {
			slog.Debug("balancer: keepalive exited", "endpoint", endpoint, "error", err)
		}
		for _, rt := range x.clone() {
			_ = rt.Close()
		}
	}()
	if err := x.Ping(ctx); err != nil {
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
	sync.RWMutex
}

// Pick picks a round tripper from the round robin balancer.
func (rr *RoundRobin) Pick(_ context.Context, _ PickInfo) (transport.RoundTripper, error) {
	rr.RLock()
	defer rr.RUnlock()
	rts := rr.rts
	if len(rts) == 0 {
		return nil, status.ResourceExhausted.Err()
	}
	idx := rr.step.Add(defaultStep) % uint32(len(rts))
	return rts[idx], nil
}

// DialContext builds a round robin balancer for endpoint: it resolves the
// endpoint to the number of backends, dials one round tripper per backend and
// starts the keepalive loop under ctx.
func DialContext(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	childCtx, cancel := context.WithCancel(ctx)
	b := rrBuilder{
		cancel: cancel,
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
	p, err := b.Build(childCtx, endpoint, opts...)
	if err != nil {
		cancel()
		return nil, err
	}
	return p, nil
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
// returns each surviving round tripper's ping result. It is the periodic health
// pass behind Keepalive.
func (rr *RoundRobin) Ping(ctx context.Context) error {
	// Resolve the endpoint to get the addresses.
	ips, err := rr.resolve(ctx)
	if err != nil {
		return err
	}
	// Create a request to send to the round trippers.
	var rts []transport.RoundTripper
	// Check the existing round trippers and remove any that are no longer valid.
	for _, rt := range rr.clone() {
		if err := rt.Ping(ctx); err != nil {
			_ = rt.Close()
			continue
		}
		if len(rts) >= len(ips) {
			_ = rt.Close()
			continue
		}
		rts = append(rts, rt)
	}
	// Create new round trippers for any new addresses.
	for i := len(rts); i < len(ips); i++ {
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
// backend round trippers itself: the keepalive loop closes every one it holds
// after the loop returns, so the connections are torn down by the goroutine that
// owned them, without racing a concurrent Ping. Close is safe to call more than
// once, and returns nil either way.
func (rr *RoundRobin) Close() error {
	if rr.cancel != nil {
		rr.cancel()
	}
	return nil
}

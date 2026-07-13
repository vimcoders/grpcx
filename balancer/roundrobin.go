package balancer

import (
	"context"
	"math/rand"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vimcoders/grpcx/status"

	"github.com/vimcoders/grpcx/roundtrip"

	"github.com/vimcoders/grpcx/resolver"

	"github.com/vimcoders/grpcx/generated/api"
)

const (
	// RoundRobinName is the name of round robin balancer.
	RoundRobinName = "round_robin"
	// defaultStep is the default step of round robin balancer.
	defaultStep = 1
	// defaultKeepalive is the default keepalive interval of round robin balancer.
	defaultKeepalive = time.Minute
	// defaultKeepaliveTimeout is the default keepalive timeout of round robin balancer.
	defaultKeepaliveTimeout = time.Second * 5
)

// rrBuilder is the builder of round robin balancer.
type rrBuilder struct{}

// Build builds a round robin balancer.
func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...roundtrip.Option) (Picker, error) {
	// Create a child context that can be canceled when the balancer is closed.
	childCtx, cancel := context.WithCancel(ctx)
	// Create a round robin balancer.
	var x = RoundRobin{
		dialContext: func(ctx context.Context) (roundtrip.RoundTripper, error) {
			return roundtrip.DialContext(ctx, endpoint, opts...)
		},
		resolveContext: func(ctx context.Context) ([]resolver.Address, error) {
			resolver := resolver.GetResolver("dns")
			address, err := resolver.Resolve(url.URL{Scheme: "dns", Host: endpoint})
			if err != nil {
				return nil, err
			}
			return address, nil
		},
		cancelFunc: cancel,
	}
	// Resolve the endpoint to get the addresses.
	address, err := x.resolveContext(ctx)
	if err != nil {
		return nil, err
	}
	// Dial to each address and create a round tripper for each.
	for range address {
		rt, err := x.dialContext(ctx)
		if err != nil {
			return nil, err
		}
		x.rts = append(x.rts, rt)
	}
	// Randomly select the next round tripper to use.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	next := uint32(rng.Intn(len(x.rts)))
	x.next.Store(next)
	go func() {
		if err := x.Keepalive(childCtx); err != nil {
			return
		}
	}()
	// Return the round robin balancer.
	return &x, nil
}

// RoundRobin is a round robin balancer.
type RoundRobin struct {
	rts            []roundtrip.RoundTripper
	next           atomic.Uint32
	dialContext    func(ctx context.Context) (roundtrip.RoundTripper, error)
	resolveContext func(ctx context.Context) ([]resolver.Address, error)
	cancelFunc     context.CancelFunc
	sync.RWMutex
}

// Pick picks a round tripper from the round robin balancer.
func (rr *RoundRobin) Pick(_ context.Context, _ PickInfo) (roundtrip.RoundTripper, error) {
	rr.RLock()
	defer rr.RUnlock()
	rts := rr.rts
	if len(rts) == 0 {
		return nil, status.ResourceExhausted.Err()
	}
	idx := rr.next.Add(defaultStep) % uint32(len(rts))
	return rts[idx], nil
}

// DialContext dials a round robin balancer.
func DialContext(ctx context.Context, endpoint string, opts ...roundtrip.Option) (Picker, error) {
	var b rrBuilder
	return b.Build(ctx, endpoint, opts...)
}

// Keepalive keeps the round robin balancer alive.
func (rr *RoundRobin) Keepalive(ctx context.Context) error {
	ticker := time.NewTicker(defaultKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		case <-ticker.C:
			_ = rr.keepalive(ctx)
		}
	}
}

// keepalive keeps the round robin balancer alive.
func (rr *RoundRobin) keepalive(ctx context.Context) error {
	// Create a timeout context for the keepalive.
	timeoutCtx, cancel := context.WithTimeout(ctx, defaultKeepaliveTimeout)
	defer cancel()

	rr.Lock()
	defer rr.Unlock()
	// Resolve the endpoint to get the addresses.
	ips, err := rr.resolveContext(timeoutCtx)
	if err != nil {
		return err
	}
	// Create a request to send to the round trippers.
	req := &api.Request{}
	var rts []roundtrip.RoundTripper
	// Check the existing round trippers and remove any that are no longer valid.
	for _, rt := range rr.rts {
		if _, err := rt.RoundTrip(timeoutCtx, req); err != nil {
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
		rt, err := rr.dialContext(timeoutCtx)
		if err != nil {
			continue
		}
		rts = append(rts, rt)
	}
	// Update the round tripper list.
	rr.rts = rts
	return nil
}

// Close closes the round robin balancer.
func (rr *RoundRobin) Close() error {
	rr.Lock()
	defer rr.Unlock()
	// Cancel the context to stop the keepalive goroutine.
	if rr.cancelFunc != nil {
		rr.cancelFunc()
	}
	// 	Close all the round trippers.
	rts := rr.rts
	for i := range rts {
		_ = rts[i].Close()
	}
	return nil
}

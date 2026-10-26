package balancer

import (
	"context"
	"math/rand"
	"net/url"
	"sync"
	"sync/atomic"
	"time"

	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"

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
)

// rrBuilder is the builder of round robin balancer.
type rrBuilder struct{}

// Build builds a round robin balancer.
func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	// Create a child context that can be canceled when the balancer is closed.
	childCtx, cancel := context.WithCancel(ctx)
	// Create a round robin balancer.
	var x = RoundRobin{
		dialContext: func(ctx context.Context) (transport.RoundTripper, error) {
			return transport.DialContext(ctx, endpoint, opts...)
		},
		resolveContext: func(ctx context.Context) (int, error) {
			resolver := resolver.GetResolver("dns")
			address, err := resolver.Resolve(url.URL{Scheme: "dns", Host: endpoint})
			if err != nil {
				return 0, err
			}
			return len(address), nil
		},
		cancelFunc: cancel,
	}
	// Resolve the endpoint to get the addresses.
	podCount, err := x.resolveContext(ctx)
	if err != nil {
		return nil, err
	}
	// Dial to each address and create a round tripper for each.
	for range podCount {
		rt, err := x.dialContext(ctx)
		if err != nil {
			x.Close()
			return nil, err
		}
		x.rts = append(x.rts, rt)
	}
	// Randomly select the next round tripper to use.
	rng := rand.New(rand.NewSource(time.Now().UnixNano()))
	count := uint32(rng.Intn(len(x.rts)))
	x.count.Store(count)
	go x.Keepalive(childCtx)
	// Return the round robin balancer.
	return &x, nil
}

// RoundRobin is a round robin balancer.
type RoundRobin struct {
	rts            []transport.RoundTripper
	count          atomic.Uint32
	dialContext    func(ctx context.Context) (transport.RoundTripper, error)
	resolveContext func(ctx context.Context) (int, error)
	cancelFunc     context.CancelFunc
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
	idx := rr.count.Add(defaultStep) % uint32(len(rts))
	return rts[idx], nil
}

// DialContext dials a round robin balancer.
func DialContext(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	var b rrBuilder
	p, err := b.Build(ctx, endpoint, opts...)
	if err != nil {
		return nil, err
	}
	return p, nil
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
			_ = rr.Ping(ctx)
		}
	}
}

func (rr *RoundRobin) Ping(ctx context.Context) error {
	// Resolve the endpoint to get the addresses.
	podCount, err := rr.resolveContext(ctx)
	if err != nil {
		return err
	}
	// Create a request to send to the round trippers.
	req := &api.Request{}
	var rts []transport.RoundTripper
	// Check the existing round trippers and remove any that are no longer valid.
	for _, rt := range rr.clone() {
		if _, err := rt.RoundTrip(ctx, req); err != nil {
			_ = rt.Close()
			continue
		}
		if len(rts) >= podCount {
			_ = rt.Close()
			continue
		}
		rts = append(rts, rt)
	}
	// Create new round trippers for any new addresses.
	for i := len(rts); i < podCount; i++ {
		rt, err := rr.dialContext(ctx)
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

// Close closes the round robin balancer.
func (rr *RoundRobin) Close() error {
	// Cancel the context to stop the keepalive goroutine.
	if rr.cancelFunc != nil {
		rr.cancelFunc()
	}
	// 	Close all the round trippers.
	for _, rt := range rr.clone() {
		_ = rt.Close()
	}
	return nil
}

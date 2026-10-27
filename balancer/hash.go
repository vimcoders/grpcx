package balancer

import (
	"context"
	"net/url"
	"sync"
	"time"

	"github.com/vimcoders/grpcx/resolver"
	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"
)

type hashBuilder struct{}

// Build builds a round robin balancer.
func (h *hashBuilder) Build(ctx context.Context, step int64, endpoint string, opts ...transport.Option) (Picker, error) {
	// Create a child context that can be canceled when the balancer is closed.
	childCtx, cancel := context.WithCancel(ctx)
	// Create a round robin balancer.
	var x = hashRing{
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
		step:       step,
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
		x.ring = append(x.ring, rt)
	}
	go x.Keepalive(childCtx)
	// Return the round robin balancer.
	return &x, nil
}

type hashRing struct {
	ring           []transport.RoundTripper
	dialContext    func(ctx context.Context) (transport.RoundTripper, error)
	resolveContext func(ctx context.Context) (int, error)
	cancelFunc     context.CancelFunc
	step           int64
	sync.RWMutex
}

func (h *hashRing) Pick(_ context.Context, info PickInfo) (transport.RoundTripper, error) {
	h.RLock()
	defer h.RUnlock()
	if len(h.ring) == 0 {
		return nil, status.ResourceExhausted.Err()
	}
	bucket := info.Authority / h.step
	if bucket >= int64(len(h.ring)) {
		return nil, status.OutOfRange.Err()
	}
	return h.ring[bucket], nil
}

func (h *hashRing) Ping(ctx context.Context) error {
	// Resolve the endpoint to get the addresses.
	podCount, err := h.resolveContext(ctx)
	if err != nil {
		return err
	}
	// Create a request to send to the round trippers.
	var ring []transport.RoundTripper
	// Check the existing round trippers and remove any that are no longer valid.
	for _, rt := range h.clone() {
		if err := rt.Ping(ctx); err != nil {
			_ = rt.Close()
			continue
		}
		if len(ring) >= podCount {
			_ = rt.Close()
			continue
		}
		ring = append(ring, rt)
	}
	// Create new round trippers for any new addresses.
	for i := len(ring); i < podCount; i++ {
		rt, err := h.dialContext(ctx)
		if err != nil {
			continue
		}
		ring = append(ring, rt)
	}
	h.Lock()
	defer h.Unlock()
	// Update the round tripper list.
	h.ring = ring
	return nil
}

func (h *hashRing) clone() []transport.RoundTripper {
	h.RLock()
	defer h.RUnlock()
	return append([]transport.RoundTripper{}, h.ring...)
}

// Close closes the round robin balancer.
func (h *hashRing) Close() error {
	// Cancel the context to stop the keepalive goroutine.
	if h.cancelFunc != nil {
		h.cancelFunc()
	}
	// 	Close all the round trippers.
	for _, rt := range h.clone() {
		_ = rt.Close()
	}
	return nil
}

// Keepalive keeps the round robin balancer alive.
func (h *hashRing) Keepalive(ctx context.Context) error {
	ticker := time.NewTicker(defaultKeepalive)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return status.Canceled.Err()
		case <-ticker.C:
			_ = h.Ping(ctx)
		}
	}
}

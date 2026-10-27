package balancer

import (
	"context"
	"sort"
	"sync"

	"github.com/vimcoders/grpcx/status"
	"github.com/vimcoders/grpcx/transport"
)

type hashRing struct {
	ring           []RoundTripper // 排序后的虚拟节点
	dialContext    func(ctx context.Context) (RoundTripper, error)
	resolveContext func(ctx context.Context) (int, error)
	cancelFunc     context.CancelFunc
	step           int64
	sync.RWMutex
}

type RoundTripper struct {
	hash int64
	transport.RoundTripper
}

func (h *hashRing) Pick(_ context.Context, info PickInfo) (transport.RoundTripper, error) {
	h.RLock()
	defer h.RUnlock()
	if len(h.ring) == 0 {
		return nil, status.ResourceExhausted.Err()
	}
	i := sort.Search(len(h.ring), func(i int) bool {
		return h.ring[i].hash >= info.Authority
	})
	if i == len(h.ring) {
		return h.ring[i%len(h.ring)], nil
	}
	return h.ring[i], nil
}

func (h *hashRing) Ping(ctx context.Context) error {
	// Resolve the endpoint to get the addresses.
	podCount, err := h.resolveContext(ctx)
	if err != nil {
		return err
	}
	// Create a request to send to the round trippers.
	var ring []RoundTripper
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
		rt.hash = h.step * int64(i)
		ring = append(ring, rt)
	}
	sort.Slice(ring, func(i, j int) bool { return ring[i].hash < ring[j].hash })
	h.Lock()
	defer h.Unlock()
	// Update the round tripper list.
	h.ring = ring
	return nil
}

func (h *hashRing) clone() []RoundTripper {
	h.RLock()
	defer h.RUnlock()
	return append([]RoundTripper{}, h.ring...)
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

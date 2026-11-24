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
	"google.golang.org/grpc/codes"
)

const (
	// RoundRobinName is the name of round robin balancer.
	RoundRobinName = "round_robin"
	// defaultStep is the default step of round robin balancer.
	defaultStep = 1
)

// rrBuilder is the builder of round robin balancer.
type rrBuilder struct{}

// Build builds a round robin balancer.
func (b *rrBuilder) Build(ctx context.Context, endpoint string, opts ...transport.Option) (Picker, error) {
	// Create a round robin balancer.
	var x = RoundRobin{
		dial: func(childCtx context.Context) (transport.RoundTripper, error) {
			return transport.DialContext(childCtx, endpoint, opts...)
		},
		resolve: func(childCtx context.Context) (int, error) {
			resolver := resolver.GetResolver("dns")
			address, err := resolver.Resolve(childCtx, url.URL{Scheme: "dns", Host: endpoint})
			if err != nil {
				return 0, err
			}
			return len(address), nil
		},
	}
	// Resolve the endpoint to get the addresses.
	podCount, err := x.resolve(ctx)
	if err != nil {
		return nil, err
	}
	if podCount == 0 {
		return nil, status.Unavailable.Err()
	}
	// Dial to each address and create a round tripper for each.
	for range podCount {
		rt, err := x.dial(ctx)
		if err != nil {
			// Build is already failing with err; the half-built round robin is
			// discarded, and its close error cannot change that.
			_ = x.Close()
			return nil, err
		}
		x.rts = append(x.rts, rt)
	}
	go func() {
		// The keepalive loop ends with the balancer's context, so only an
		// unexpected exit is worth reporting.
		if err := x.Keepalive(ctx, opts...); status.Code(err) != codes.Canceled {
			slog.Error("balancer: keepalive exited", "endpoint", endpoint, "error", err)
		}
	}()
	// Return the round robin balancer.
	return &x, nil
}

// RoundRobin is a round robin balancer.
type RoundRobin struct {
	rts     []transport.RoundTripper
	count   atomic.Uint32
	dial    func(context.Context) (transport.RoundTripper, error)
	resolve func(context.Context) (int, error)
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
	idx := (rr.count.Add(defaultStep) - 1) % uint32(len(rts))
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
	podCount, err := rr.resolve(ctx)
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
		if len(rts) >= podCount {
			_ = rt.Close()
			continue
		}
		rts = append(rts, rt)
	}
	// Create new round trippers for any new addresses.
	for i := len(rts); i < podCount; i++ {
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

// Close closes the round robin balancer.
func (rr *RoundRobin) Close() error {
	for _, rt := range rr.clone() {
		_ = rt.Close()
	}
	return nil
}

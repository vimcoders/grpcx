package resolver

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"testing"
)

// lastLookupCtx is the context the stubbed lookup was called with.
var lastLookupCtx context.Context

// stubLookup replaces the package lookup hook for one test and restores it
// afterwards, so a resolve never touches the network.
func stubLookup(t *testing.T, answer []string, err error) {
	t.Helper()
	previous := lookupHost
	lookupHost = func(ctx context.Context, _ string) ([]string, error) {
		lastLookupCtx = ctx
		return answer, err
	}
	t.Cleanup(func() {
		lookupHost = previous
		lastLookupCtx = nil
	})
}

// TestResolveRejectsAnEmptyHost covers a target without a host: it is an error,
// never an empty address list.
func TestResolveRejectsAnEmptyHost(t *testing.T) {
	_, err := GetResolver("dns").Resolve(context.Background(), url.URL{})
	if err == nil || !strings.Contains(err.Error(), "empty host") {
		t.Errorf("Resolve(empty host) = %v, want the empty-host error", err)
	}
}

// TestResolvePairsEveryAddressWithThePort covers the mapping: each answered IP
// becomes host:port, and the original host is kept as the server name.
func TestResolvePairsEveryAddressWithThePort(t *testing.T) {
	stubLookup(t, []string{"10.0.0.1", "10.0.0.2"}, nil)

	addrs, err := GetResolver("dns").Resolve(context.Background(), url.URL{Scheme: "dns", Host: "svc.local:9090"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("Resolve returned %d addresses, want 2", len(addrs))
	}
	if got, want := addrs[0].Addr, net.JoinHostPort("10.0.0.1", "9090"); got != want {
		t.Errorf("Addr = %q, want %q", got, want)
	}
	if got, want := addrs[1].ServerName, "svc.local"; got != want {
		t.Errorf("ServerName = %q, want %q", got, want)
	}
}

// TestResolveKeepsThePortEmptyWhenTheTargetHasNone covers a target without a
// port: the address still pairs the IP with an empty port.
func TestResolveKeepsThePortEmptyWhenTheTargetHasNone(t *testing.T) {
	stubLookup(t, []string{"10.0.0.1"}, nil)

	addrs, err := GetResolver("dns").Resolve(context.Background(), url.URL{Host: "svc.local"})
	if err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if got, want := addrs[0].Addr, net.JoinHostPort("10.0.0.1", ""); got != want {
		t.Errorf("Addr = %q, want %q", got, want)
	}
}

// TestResolvePropagatesTheLookupError covers a failed DNS answer.
func TestResolvePropagatesTheLookupError(t *testing.T) {
	want := errors.New("dns: server misbehaving")
	stubLookup(t, nil, want)

	if _, err := GetResolver("dns").Resolve(context.Background(), url.URL{Host: "svc.local"}); !errors.Is(err, want) {
		t.Errorf("Resolve = %v, want the lookup error", err)
	}
}

// TestResolveRejectsAnEmptyAnswer covers a host that resolves to nothing: a
// caller must not dial zero backends by accident.
func TestResolveRejectsAnEmptyAnswer(t *testing.T) {
	stubLookup(t, nil, nil)

	_, err := GetResolver("dns").Resolve(context.Background(), url.URL{Host: "svc.local"})
	if err == nil || !strings.Contains(err.Error(), "no addresses found") {
		t.Errorf("Resolve(empty answer) = %v, want the no-addresses error", err)
	}
}

// TestResolvePassesTheCallerContextToTheLookup covers the cancellation path: the
// context the caller passed is the one the lookup is bounded by.
func TestResolvePassesTheCallerContextToTheLookup(t *testing.T) {
	stubLookup(t, []string{"10.0.0.1"}, nil)
	ctx := t.Context()

	if _, err := GetResolver("dns").Resolve(ctx, url.URL{Host: "svc.local"}); err != nil {
		t.Fatalf("Resolve: %v", err)
	}
	if lastLookupCtx != ctx {
		t.Error("Resolve did not forward the caller's context to the lookup")
	}
}

// TestGetResolverAlwaysAnswersWithDNS covers the scheme switch: every scheme
// resolves through the DNS resolver, and it holds nothing to close.
func TestGetResolverAlwaysAnswersWithDNS(t *testing.T) {
	stubLookup(t, []string{"10.0.0.1"}, nil)

	r := GetResolver("anything")
	if _, err := r.Resolve(context.Background(), url.URL{Host: "svc.local"}); err != nil {
		t.Errorf("Resolve through GetResolver = %v, want it to use DNS", err)
	}
	if err := r.Close(); err != nil {
		t.Errorf("Close = %v, want nil", err)
	}
}

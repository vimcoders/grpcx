package resolver_test

import (
	"net/url"
	"testing"

	"github.com/vimcoders/grpcx/resolver"
)

// TestResolveLiteralHost resolves a literal address: the port must be carried
// onto every address and the server name must be the host that was asked for.
func TestResolveLiteralHost(t *testing.T) {
	res := resolver.GetResolver("dns")
	u, err := url.Parse("dns://127.0.0.1:50002")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	addrs, err := res.Resolve(t.Context(), *u)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(addrs) == 0 {
		t.Fatal("resolve returned no addresses")
	}
	for _, a := range addrs {
		if got, want := a.Addr, "127.0.0.1:50002"; got != want {
			t.Fatalf("address = %q, want %q", got, want)
		}
		if got, want := a.ServerName, "127.0.0.1"; got != want {
			t.Fatalf("server name = %q, want %q", got, want)
		}
	}
}

// TestResolveEmptyHost is the input guard: a target without a host is an error,
// not an empty address list.
func TestResolveEmptyHost(t *testing.T) {
	res := resolver.GetResolver("dns")
	if _, err := res.Resolve(t.Context(), url.URL{Scheme: "dns"}); err == nil {
		t.Fatal("resolve without a host succeeded, want an error")
	}
}

// TestResolverClose is the idle path: closing a DNS resolver must not fail.
func TestResolverClose(t *testing.T) {
	if err := resolver.GetResolver("dns").Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
}

package resolver

import (
	"context"
	"errors"
	"net/url"
	"testing"
)

// TestResolvePassesContextThrough pins the contract the ctx parameter exists for:
// a canceled context reaches the lookup, and its error is what Resolve returns.
func TestResolvePassesContextThrough(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var gotCtx context.Context
	restore := lookupHost
	lookupHost = func(ctx context.Context, host string) ([]string, error) {
		gotCtx = ctx
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		// A response here would prove the canceled context was ignored.
		return []string{"10.0.0.1"}, nil
	}
	t.Cleanup(func() { lookupHost = restore })

	u, err := url.Parse("dns://api.example.com:50051")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	_, err = newDNSResolver().Resolve(ctx, *u)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("resolve error = %v, want context.Canceled", err)
	}
	if gotCtx != ctx {
		t.Fatal("resolve did not pass the caller's context to the lookup")
	}
}

// TestResolvePairsPortWithEveryAddress checks the mapping from a lookup result
// to addresses: one entry per IP, each carrying the port and the server name.
func TestResolvePairsPortWithEveryAddress(t *testing.T) {
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) {
		return []string{"10.0.0.1", "10.0.0.2"}, nil
	}
	t.Cleanup(func() { lookupHost = restore })

	u, err := url.Parse("dns://api.example.com:50051")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	addrs, err := newDNSResolver().Resolve(context.Background(), *u)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if len(addrs) != 2 {
		t.Fatalf("addresses = %d, want 2", len(addrs))
	}
	for i, want := range []string{"10.0.0.1:50051", "10.0.0.2:50051"} {
		if addrs[i].Addr != want {
			t.Fatalf("address[%d] = %q, want %q", i, addrs[i].Addr, want)
		}
		if addrs[i].ServerName != "api.example.com" {
			t.Fatalf("server name[%d] = %q, want api.example.com", i, addrs[i].ServerName)
		}
	}
}

// TestResolveEmptyLookupIsAnError covers a lookup that answers with no records:
// the caller must see an error rather than a backend-less round robin.
func TestResolveEmptyLookupIsAnError(t *testing.T) {
	restore := lookupHost
	lookupHost = func(context.Context, string) ([]string, error) { return nil, nil }
	t.Cleanup(func() { lookupHost = restore })

	if _, err := newDNSResolver().Resolve(context.Background(), url.URL{Host: "api.example.com:50051"}); err == nil {
		t.Fatal("resolve with no records succeeded, want an error")
	}
}

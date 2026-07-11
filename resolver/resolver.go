package resolver

import (
	"context"
	"errors"
	"net"
	"net/url"

	"google.golang.org/grpc/resolver"
)

// Address is an alias for resolver.Address.
type Address resolver.Address

// Resolver is the interface for resolving a target into a list of addresses.
type Resolver interface {
	Resolve(url.URL) ([]Address, error)
	Watch(context.Context, url.URL) (<-chan []Address, error)
	Close() error
}

// dnsResolver is a resolver that resolves a target into a list of addresses using DNS.
type dnsResolver struct{}

// Resolve resolves a target into a list of addresses using DNS.
func newDNSResolver() *dnsResolver {
	return &dnsResolver{}
}

// Resolve resolves a target into a list of addresses using DNS.
func (d *dnsResolver) Resolve(u url.URL) ([]Address, error) {
	hostName := u.Hostname()
	if hostName == "" {
		return nil, errors.New("resolver: empty host")
	}
	ips, err := net.LookupHost(hostName)
	if err != nil {
		return nil, err
	}

	var addrs []Address
	for _, ip := range ips {
		addrs = append(addrs, Address{
			Addr:       net.JoinHostPort(ip, u.Port()),
			ServerName: hostName,
		})
	}
	if len(addrs) == 0 {
		return nil, errors.New("resolver: no addresses found")
	}
	return addrs, nil
}

// Watch watches for changes to the list of addresses for a target. It returns a channel that will receive updates to the list of addresses. The channel will be closed when the context is canceled or when the resolver is closed.
func (d *dnsResolver) Watch(ctx context.Context, u url.URL) (<-chan []Address, error) {
	// TODO: Implement a watcher that periodically resolves the target and sends updates to the channel when the list of addresses changes.
	return nil, nil
}

// Close closes the resolver and releases any resources associated with it.
func (d *dnsResolver) Close() error {
	return nil
}

// Builder is the interface for building a resolver.
type Builder interface {
	Build() (Resolver, error)
	Scheme() string
}

// GetResolver returns a resolver for the given scheme. If no resolver is registered for the scheme, it returns a DNS resolver.
func GetResolver(scheme string) Resolver {
	return newDNSResolver()
}

// func addressesEqual(a, b []Address) bool {
// 	if len(a) != len(b) {
// 		return false
// 	}
// 	as := make([]string, len(a))
// 	bs := make([]string, len(b))
// 	for i := range a {
// 		as[i] = a[i].Addr
// 		bs[i] = b[i].Addr
// 	}
// 	sort.Strings(as)
// 	sort.Strings(bs)
// 	for i := range as {
// 		if as[i] != bs[i] {
// 			return false
// 		}
// 	}
// 	return true
// }

package resolver

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sort"

	"google.golang.org/grpc/resolver"
)

type Address resolver.Address

type Resolver interface {
	Resolve(url.URL) ([]Address, error)
	Watch(context.Context, url.URL) (<-chan []Address, error)
	Close() error
}

type dnsResolver struct {
}

func newDNSResolver() *dnsResolver {
	return &dnsResolver{}
}

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

func (d *dnsResolver) Watch(ctx context.Context, u url.URL) (<-chan []Address, error) {
	ch := make(chan []Address, 1)
	return ch, nil
}

func (d *dnsResolver) Close() error {
	return nil
}

type Builder interface {
	Build() (Resolver, error)
	Scheme() string
}

func GetResolver(scheme string) Resolver {
	return newDNSResolver()
}

func addressesEqual(a, b []Address) bool {
	if len(a) != len(b) {
		return false
	}
	as := make([]string, len(a))
	bs := make([]string, len(b))
	for i := range a {
		as[i] = a[i].Addr
		bs[i] = b[i].Addr
	}
	sort.Strings(as)
	sort.Strings(bs)
	for i := range as {
		if as[i] != bs[i] {
			return false
		}
	}
	return true
}

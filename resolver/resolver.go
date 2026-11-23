/*
   Copyright The containerd Authors.

   Licensed under the Apache License, Version 2.0 (the "License");
   you may not use this file except in compliance with the License.
   You may obtain a copy of the License at

       http://www.apache.org/licenses/LICENSE-2.0

   Unless required by applicable law or agreed to in writing, software
   distributed under the License is distributed on an "AS IS" BASIS,
   WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
   See the License for the specific language governing permissions and
   limitations under the License.
*/

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

// Resolver turns a target into the list of addresses it currently resolves to.
type Resolver interface {
	// Resolve looks the target up. The context bounds the lookup, so a caller
	// with a deadline or a cancellation never waits on a stuck DNS server.
	Resolve(context.Context, url.URL) ([]Address, error)
	// Close releases the resources the resolver holds.
	Close() error
}

// dnsResolver resolves a target through the process DNS configuration.
type dnsResolver struct{}

// lookupHost is net.DefaultResolver.LookupHost, indirected so a test can answer
// without touching the network and pin that the caller's context reaches it.
var lookupHost = net.DefaultResolver.LookupHost

// newDNSResolver returns a resolver that answers from DNS.
func newDNSResolver() *dnsResolver {
	return &dnsResolver{}
}

// Resolve looks the host of u up and pairs every address with the port u
// carries. An empty host, or a host that resolves to nothing, is an error rather
// than an empty list, so a caller never dials zero backends by accident.
func (d *dnsResolver) Resolve(ctx context.Context, u url.URL) ([]Address, error) {
	hostName := u.Hostname()
	if hostName == "" {
		return nil, errors.New("resolver: empty host")
	}
	ips, err := lookupHost(ctx, hostName)
	if err != nil {
		return nil, err
	}

	addrs := make([]Address, 0, len(ips))
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

// Close releases the resources of the resolver. The DNS resolver holds none.
func (d *dnsResolver) Close() error {
	return nil
}

// GetResolver returns a resolver for the given scheme. DNS is the only one
// implemented, so every scheme resolves through DNS for now.
func GetResolver(string) Resolver {
	return newDNSResolver()
}

package discovery

import (
	"net"
)

type Result struct {
	Cacheable bool
	CacheKey  string
	Instances []Instance
}

// Change contains the difference between the current discovery result and the previous one.
// It is designed for providing detail information when dispatching an event for service
// discovery result change.
// Since the loadbalancer may rely on caching the result of resolver to improve performance,
// the resolver implementation should dispatch an event when result changes.
type Change struct {
	Result  Result
	Added   []Instance
	Updated []Instance
	Removed []Instance
}

type Instance interface {
	Address() net.Addr
	Weight() int
	Tag(key string) (value string, ok bool)
}

func NewInstance(addr net.Addr, weight int, tags map[string]string) Instance {
	return &instance{
		addr:   addr,
		weight: weight,
		tags:   tags,
	}
}

type instance struct {
	addr   net.Addr
	weight int
	tags   map[string]string
}

func (x *instance) Address() net.Addr {
	return x.addr
}

func (x *instance) Weight() int {
	return x.weight
}

func (x *instance) Tag(key string) (value string, ok bool) {
	value, ok = x.tags[key]
	return value, ok
}

func (x *instance) Tags() map[string]string {
	return x.tags
}

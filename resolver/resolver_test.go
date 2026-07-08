package resolver_test

import (
	"grpcx/resolver"
	"net/url"
	"testing"
)

func TestResolver(t *testing.T) {
	res := resolver.GetResolver("")
	u := url.URL{Scheme: "dns", Host: "127.0.0.1:50002"}
	address, err := res.Resolve(u)
	if err != nil {
		t.Error(err)
		return
	}
	t.Log(address)
}

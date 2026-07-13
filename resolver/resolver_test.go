package resolver_test

import (
	"net/url"
	"testing"

	"github.com/vimcoders/grpcx/resolver"
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

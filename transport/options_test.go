package transport

import (
	"testing"
	"time"

	"github.com/vimcoders/grpcx/generated/api"
)

// TestDefaultOptionsArePackageDefaults covers every field DefaultOptions fills
// in, including the ones no Option can reach.
func TestDefaultOptionsArePackageDefaults(t *testing.T) {
	o := DefaultOptions()
	if o.timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", o.timeout, defaultTimeout)
	}
	if o.readTimeout != defaultReadTimeout {
		t.Errorf("readTimeout = %v, want %v", o.readTimeout, defaultReadTimeout)
	}
	if o.writeTimeout != defaultWriteTimeout {
		t.Errorf("writeTimeout = %v, want %v", o.writeTimeout, defaultWriteTimeout)
	}
	if o.keepalive != defaultKeepalive {
		t.Errorf("keepalive = %v, want %v", o.keepalive, defaultKeepalive)
	}
	if o.maxStreams != defaultMaxStreams {
		t.Errorf("maxStreams = %d, want %d", o.maxStreams, defaultMaxStreams)
	}
	if o.bufsize != 0 {
		t.Errorf("bufsize = %d, want batching off", o.bufsize)
	}
	if o.ecodec == nil {
		t.Error("ecodec is nil")
	}
	if o.Keepalive() != defaultKeepalive {
		t.Errorf("Keepalive() = %v, want %v", o.Keepalive(), defaultKeepalive)
	}
	if o.Timeout() != defaultTimeout {
		t.Errorf("Timeout() = %v, want %v", o.Timeout(), defaultTimeout)
	}
}

// TestWithBufSizeCapsAtTheMaximum covers the ceiling and the pass-through of
// values below it, including the non-positive ones that turn batching off.
func TestWithBufSizeCapsAtTheMaximum(t *testing.T) {
	cases := []struct {
		name string
		in   int
		want int
	}{
		{"below ceiling", 4096, 4096},
		{"at ceiling", defaultMaxBufSize, defaultMaxBufSize},
		{"above ceiling", defaultMaxBufSize * 4, defaultMaxBufSize},
		{"zero", 0, 0},
		{"negative", -1, -1},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			o := DefaultOptions()
			WithBufSize(c.in)(&o)
			if o.bufsize != c.want {
				t.Errorf("bufsize = %d, want %d", o.bufsize, c.want)
			}
		})
	}
}

// TestWithCodecStoresNilAsGiven covers the absence of a nil check: the field
// holds whatever was passed.
func TestWithCodecStoresNilAsGiven(t *testing.T) {
	o := DefaultOptions()
	WithCodec(nil)(&o)
	if o.ecodec != nil {
		t.Errorf("ecodec = %v, want nil stored as given", o.ecodec)
	}
}

// TestZeroValuesFallBackToDefaults covers the cmp.Or guards on the options that
// replace zero with the package default.
func TestZeroValuesFallBackToDefaults(t *testing.T) {
	o := DefaultOptions()
	WithMaxStreams(0)(&o)
	WithTimeout(0)(&o)
	WithReadTimeout(0)(&o)
	WithWriteTimeout(0)(&o)
	if o.maxStreams != defaultMaxStreams {
		t.Errorf("maxStreams = %d, want %d", o.maxStreams, defaultMaxStreams)
	}
	if o.timeout != defaultTimeout {
		t.Errorf("timeout = %v, want %v", o.timeout, defaultTimeout)
	}
	if o.readTimeout != defaultReadTimeout {
		t.Errorf("readTimeout = %v, want %v", o.readTimeout, defaultReadTimeout)
	}
	if o.writeTimeout != defaultWriteTimeout {
		t.Errorf("writeTimeout = %v, want %v", o.writeTimeout, defaultWriteTimeout)
	}
}

// TestNonZeroValuesReplaceDefaults covers the other side of the same guard.
func TestNonZeroValuesReplaceDefaults(t *testing.T) {
	o := DefaultOptions()
	WithMaxStreams(3)(&o)
	WithTimeout(time.Second)(&o)
	WithReadTimeout(2 * time.Second)(&o)
	WithWriteTimeout(3 * time.Second)(&o)
	if o.maxStreams != 3 {
		t.Errorf("maxStreams = %d, want 3", o.maxStreams)
	}
	if o.timeout != time.Second {
		t.Errorf("timeout = %v, want 1s", o.timeout)
	}
	if o.readTimeout != 2*time.Second {
		t.Errorf("readTimeout = %v, want 2s", o.readTimeout)
	}
	if o.writeTimeout != 3*time.Second {
		t.Errorf("writeTimeout = %v, want 3s", o.writeTimeout)
	}
}

// TestDefaultServerOptionsIsFreshPerCall covers why DefaultServerOptions is a
// function: each server owns its own Codec, and the defaults are rebuilt each
// time.
func TestDefaultServerOptionsIsFreshPerCall(t *testing.T) {
	first := DefaultServerOptions()
	second := DefaultServerOptions()
	if first.Codec == second.Codec {
		t.Fatal("DefaultServerOptions shared one Codec between calls")
	}
	if _, ok := first.Codec.(*defaultCodec); !ok {
		t.Fatalf("Codec is %T, want *defaultCodec", first.Codec)
	}
	if first.desc == second.desc {
		t.Error("DefaultServerOptions shared one service descriptor")
	}
	if first.unaryInt == nil {
		t.Error("unaryInt is nil")
	}
}

// TestWithServerBufSizeChoosesTheCodec covers the branch that picks the codec
// from the threshold.
func TestWithServerBufSizeChoosesTheCodec(t *testing.T) {
	t.Run("batching on", func(t *testing.T) {
		so := serverOptions(WithServerBufSize(1))
		if so.bufsize != 1 {
			t.Fatalf("bufsize = %d, want 1", so.bufsize)
		}
		if _, ok := so.Codec.(*codec); !ok {
			t.Errorf("Codec is %T, want *codec", so.Codec)
		}
	})
	t.Run("zero", func(t *testing.T) {
		so := serverOptions(WithServerBufSize(0))
		if so.bufsize != 0 {
			t.Fatalf("bufsize = %d, want 0", so.bufsize)
		}
		if _, ok := so.Codec.(*defaultCodec); !ok {
			t.Errorf("Codec is %T, want *defaultCodec", so.Codec)
		}
	})
	t.Run("negative", func(t *testing.T) {
		so := serverOptions(WithServerBufSize(-1))
		if so.bufsize != -1 {
			t.Fatalf("bufsize = %d, want -1 stored as given", so.bufsize)
		}
		// WithBufSize stores the value as given and Send compares the pending
		// byte count against it, so a negative threshold never queues a frame:
		// batching is off. The Codec branch here checks only == 0, so a negative
		// value still installs the unpooled codec, which contradicts the
		// function's own comment and wastes the pool on every proto reply.
		if _, ok := so.Codec.(*codec); !ok {
			t.Errorf("Codec is %T, want *codec as the current branch builds it", so.Codec)
		}
	})
}

// TestServerCodecOrderIsIrrelevant covers the orderings of WithServerCodec and
// WithServerBufSize: whichever runs last re-derives the Codec from the latest
// ecodec, and the batching choice survives.
func TestServerCodecOrderIsIrrelevant(t *testing.T) {
	custom := fixedCodec{out: []byte("M")}

	cases := []struct {
		name     string
		opts     []ServerOption
		wantPool bool
	}{
		{"codec only", []ServerOption{WithServerCodec(custom)}, true},
		{"buf then codec", []ServerOption{WithServerBufSize(1), WithServerCodec(custom)}, false},
		{"codec then buf", []ServerOption{WithServerCodec(custom), WithServerBufSize(1)}, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			so := serverOptions(c.opts...)
			got, ok := so.ecodec.(fixedCodec)
			if !ok || string(got.out) != "M" {
				t.Fatalf("ecodec = %v, want the custom codec", so.ecodec)
			}
			if c.wantPool {
				if _, ok := so.Codec.(*defaultCodec); !ok {
					t.Fatalf("Codec is %T, want *defaultCodec", so.Codec)
				}
			} else if _, ok := so.Codec.(*codec); !ok {
				t.Fatalf("Codec is %T, want *codec", so.Codec)
			}
			buf, err := so.Marshal(map[string]string{"a": "1"})
			if err != nil {
				t.Fatalf("Marshal: %v", err)
			}
			defer buf.Free()
			if got, want := string(buf.ReadOnlyData()), "M"; got != want {
				t.Errorf("payload = %q, want %q", got, want)
			}
		})
	}
}

// TestRegisterServiceIgnoresNil covers the nil guard: an already registered
// service is kept when a nil descriptor or implementation arrives.
func TestRegisterServiceIgnoresNil(t *testing.T) {
	impl := &echoImpl{}
	so := serverOptions(RegisterService(&api.EchoService_ServiceDesc, impl))
	RegisterService(nil, nil)(&so)
	if so.desc.ServiceName != api.EchoService_ServiceDesc.ServiceName {
		t.Errorf("desc.ServiceName = %q, want %q", so.desc.ServiceName, api.EchoService_ServiceDesc.ServiceName)
	}
	if so.imp != impl {
		t.Error("imp was replaced by nil")
	}
}

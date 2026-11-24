package transport

import (
	"testing"
	"time"
)

// TestWithBufSizeClampsToMaxBufSize pins the block arithmetic: the option takes
// blocks of defaultBufSize, not bytes, and anything above the ceiling is cut to
// defaultMaxBufSize. A clamp that drifted would silently change batching.
func TestWithBufSizeClampsToMaxBufSize(t *testing.T) {
	cases := []struct {
		blocks int
		want   int
	}{
		{blocks: 0, want: 0},
		{blocks: 1, want: defaultBufSize},
		{blocks: 4, want: 4 * defaultBufSize},
		{blocks: 100, want: defaultMaxBufSize},
	}
	for _, c := range cases {
		o := DefaultOptions()
		WithBufSize(c.blocks)(&o)
		if o.bufsize != c.want {
			t.Errorf("WithBufSize(%d) = %d bytes, want %d", c.blocks, o.bufsize, c.want)
		}
	}
}

// TestWithServerBufSizeStoresBlocks pins the server-side unit: the option stores
// blocks of defaultBufSize clamped to defaultMaxBufSize/defaultBufSize, and
// ServerOptions.Handle converts to bytes when it builds the channel. Storing
// bytes here instead would double-apply the block conversion.
func TestWithServerBufSizeStoresBlocks(t *testing.T) {
	cases := []struct {
		blocks int
		want   int
	}{
		{blocks: 0, want: 0},
		{blocks: 1, want: 1},
		{blocks: 4, want: 4},
		{blocks: 100, want: defaultMaxBufSize / defaultBufSize},
	}
	for _, c := range cases {
		so := DefaultServerOptions
		WithServerBufSize(c.blocks)(&so)
		if so.bufsize != c.want {
			t.Errorf("WithServerBufSize(%d) stored %d blocks, want %d", c.blocks, so.bufsize, c.want)
		}
	}
}

// TestZeroOptionsFallBackToDefaults checks the documented fallbacks: a zero
// stream cap and a zero timeout mean "use the default", not "unlimited" or
// "no deadline".
func TestZeroOptionsFallBackToDefaults(t *testing.T) {
	o := DefaultOptions()
	WithMaxStreams(0)(&o)
	if o.maxStreams != defaultMaxStreams {
		t.Errorf("maxStreams = %d, want %d", o.maxStreams, uint32(defaultMaxStreams))
	}

	WithTimeout(0)(&o)
	if got := o.Timeout(); got != defaultTimeout {
		t.Errorf("Timeout() = %v, want %v", got, defaultTimeout)
	}

	WithTimeout(2 * time.Second)(&o)
	if got := o.Timeout(); got != 2*time.Second {
		t.Errorf("Timeout() = %v, want 2s", got)
	}
}

// TestServerTimeoutOptionsAreStored covers the remaining server knobs so a
// rename or a swapped field is caught here instead of in a flaky test.
func TestServerTimeoutOptionsAreStored(t *testing.T) {
	so := DefaultServerOptions
	WithServerReadTimeout(time.Second)(&so)
	WithServerWriteTimeout(2 * time.Second)(&so)

	if so.readTimeout != time.Second {
		t.Errorf("readTimeout = %v, want 1s", so.readTimeout)
	}
	if so.writeTimeout != 2*time.Second {
		t.Errorf("writeTimeout = %v, want 2s", so.writeTimeout)
	}

	WithServerCodec(so.codec)(&so)
	if so.codec == nil {
		t.Error("codec is nil after WithServerCodec")
	}
}

// TestDefaultReadTimeoutExceedsKeepalive pins the invariant the constant
// documents: the idle read deadline must stay well above the keepalive period,
// or an idle connection would be torn down between two pings. Both values are
// durations derived from defaultTimeout, so a tuning change on either side is
// caught here.
func TestDefaultReadTimeoutExceedsKeepalive(t *testing.T) {
	keepalive := DefaultOptions().Keepalive()
	if defaultReadTimeout <= keepalive {
		t.Fatalf("defaultReadTimeout = %v, keepalive = %v: the read deadline must stay above the ping period", defaultReadTimeout, keepalive)
	}
	if keepalive != 3*defaultTimeout {
		t.Fatalf("Keepalive() = %v, want %v", keepalive, 3*defaultTimeout)
	}
}

// TestKeepaliveIsAFixedMultipleOfTimeout pins that Keepalive is derived from
// defaultTimeout rather than being an independent knob, so raising the timeout
// raises the ping period with it.
func TestKeepaliveIsAFixedMultipleOfTimeout(t *testing.T) {
	if got, want := DefaultOptions().Keepalive(), defaultKeepalive; got != want {
		t.Errorf("Keepalive() = %v, want %v", got, want)
	}
}

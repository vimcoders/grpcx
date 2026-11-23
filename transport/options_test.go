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

// TestKeepaliveIsThreeCallTimeouts pins Keepalive to a multiple of the default
// timeout instead of a fixed duration, so raising the timeout also raises the
// ping period.
func TestKeepaliveIsThreeCallTimeouts(t *testing.T) {
	o := DefaultOptions()
	if got, want := o.Keepalive(), defaultKeepalive*defaultTimeout; got != want {
		t.Errorf("Keepalive() = %v, want %v", got, want)
	}
}

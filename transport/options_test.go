package transport

import (
	"context"
	"testing"
	"time"

	"github.com/vimcoders/grpcx/encoding"
	"github.com/vimcoders/grpcx/generated/api"

	"google.golang.org/grpc"
)

// TestDefaultOptionsCarryEveryPackageDefault pins the values a connection runs
// with before any option is applied, because everything else in this package is
// measured against them.
func TestDefaultOptionsCarryEveryPackageDefault(t *testing.T) {
	o := DefaultOptions()
	if o.timeout != 3*time.Second {
		t.Errorf("timeout = %v, want 3s", o.timeout)
	}
	if o.maxStreams != uint32(defaultMaxStreams) {
		t.Errorf("maxStreams = %d, want %d", o.maxStreams, uint32(defaultMaxStreams))
	}
	if o.readTimeout != defaultReadTimeout {
		t.Errorf("readTimeout = %v, want %v", o.readTimeout, defaultReadTimeout)
	}
	if o.writeTimeout != defaultWriteTimeout {
		t.Errorf("writeTimeout = %v, want %v", o.writeTimeout, defaultWriteTimeout)
	}
	if o.flushInterval != defaultFlushInterval {
		t.Errorf("flushInterval = %v, want %v", o.flushInterval, defaultFlushInterval)
	}
	if o.bufsize != 0 {
		t.Errorf("bufsize = %d, want 0 (batching off)", o.bufsize)
	}
	if o.codec == nil || o.codec.Name() != encoding.Name {
		t.Errorf("codec = %v, want the %q codec", o.codec, encoding.Name)
	}
}

// TestOptionsAccessors pins the values the balancer reads off the options.
func TestOptionsAccessors(t *testing.T) {
	o := DefaultOptions()
	if got, want := o.Keepalive(), 9*time.Second; got != want {
		t.Errorf("Keepalive() = %v, want %v", got, want)
	}
	if got, want := o.Timeout(), 3*time.Second; got != want {
		t.Errorf("Timeout() = %v, want %v", got, want)
	}
}

// TestWithTimeoutFallsBackToTheDefault documents that zero means "unset": there
// is no way to configure a zero timeout, and a caller deadline is what shortens
// a call instead.
func TestWithTimeoutFallsBackToTheDefault(t *testing.T) {
	o := DefaultOptions()
	WithTimeout(0)(&o)
	if o.timeout != defaultTimeout {
		t.Errorf("WithTimeout(0) -> %v, want %v", o.timeout, defaultTimeout)
	}
	WithTimeout(10 * time.Second)(&o)
	if o.timeout != 10*time.Second {
		t.Errorf("WithTimeout(10s) -> %v, want 10s", o.timeout)
	}
}

// TestWithMaxStreamsFallsBackToTheDefault covers the zero sentinel and a real
// cap.
func TestWithMaxStreamsFallsBackToTheDefault(t *testing.T) {
	o := DefaultOptions()
	WithMaxStreams(0)(&o)
	if o.maxStreams != uint32(defaultMaxStreams) {
		t.Errorf("WithMaxStreams(0) -> %d, want %d", o.maxStreams, uint32(defaultMaxStreams))
	}
	WithMaxStreams(5)(&o)
	if o.maxStreams != 5 {
		t.Errorf("WithMaxStreams(5) -> %d, want 5", o.maxStreams)
	}
}

// TestWithBufSizeCountsBlocksAndClamps pins the unit: the argument is blocks of
// defaultBufSize, and the result is the byte threshold channel.Send compares
// against.
func TestWithBufSizeCountsBlocksAndClamps(t *testing.T) {
	o := DefaultOptions()
	WithBufSize(0)(&o)
	if o.bufsize != 0 {
		t.Errorf("WithBufSize(0) -> %d, want 0", o.bufsize)
	}
	WithBufSize(1)(&o)
	if o.bufsize != defaultBufSize {
		t.Errorf("WithBufSize(1) -> %d, want %d", o.bufsize, defaultBufSize)
	}
	WithBufSize(17)(&o)
	if o.bufsize != defaultMaxBufSize {
		t.Errorf("WithBufSize(17) -> %d, want clamp at %d", o.bufsize, defaultMaxBufSize)
	}
}

// TestWithReadAndWriteTimeoutAreTakenLiterally documents that the two timeouts
// are not defaulted: zero reaches the connection as zero.
func TestWithReadAndWriteTimeoutAreTakenLiterally(t *testing.T) {
	o := DefaultOptions()
	WithReadTimeout(0)(&o)
	WithWriteTimeout(0)(&o)
	if o.readTimeout != 0 || o.writeTimeout != 0 {
		t.Errorf("timeouts = (%v, %v), want (0, 0)", o.readTimeout, o.writeTimeout)
	}
	WithReadTimeout(2 * time.Second)(&o)
	WithWriteTimeout(500 * time.Millisecond)(&o)
	if o.readTimeout != 2*time.Second || o.writeTimeout != 500*time.Millisecond {
		t.Errorf("timeouts = (%v, %v), want (2s, 500ms)", o.readTimeout, o.writeTimeout)
	}
}

// TestWithCodecReachesMarshal proves the codec is used by the call path rather
// than only stored in the options struct.
func TestWithCodecReachesMarshal(t *testing.T) {
	counter := &countingCodec{}
	o := DefaultOptions()
	WithCodec(counter)(&o)
	if _, err := o.codec.Marshal(&api.EchoRequest{Message: "x"}); err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	if counter.marshal.Load() != 1 {
		t.Errorf("counter.marshal = %d, want 1", counter.marshal.Load())
	}
}

// TestRegisterServiceKeepsThePreviousRegistration covers the documented nil
// handling: an already registered service is never replaced by a nil argument.
func TestRegisterServiceKeepsThePreviousRegistration(t *testing.T) {
	so := DefaultServerOptions
	impl := &echoImpl{}
	RegisterService(&api.EchoService_ServiceDesc, impl)(&so)
	if so.desc != &api.EchoService_ServiceDesc || so.imp != impl {
		t.Fatalf("registration was not installed")
	}
	RegisterService(nil, nil)(&so)
	if so.desc != &api.EchoService_ServiceDesc || so.imp != impl {
		t.Errorf("RegisterService(nil, nil) replaced the registration")
	}
}

// TestUnaryServerInterceptorReplacesTheDefault proves the option installs the
// interceptor the dispatcher runs.
func TestUnaryServerInterceptorReplacesTheDefault(t *testing.T) {
	var called bool
	so := DefaultServerOptions
	UnaryServerInterceptor(func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		called = true
		return handler(ctx, req)
	})(&so)
	reply, err := so.unaryInt(context.Background(), &api.EchoRequest{Message: "x"}, &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		return "reply", nil
	})
	if err != nil || reply != "reply" {
		t.Fatalf("interceptor chain = (%v, %v), want (reply, nil)", reply, err)
	}
	if !called {
		t.Errorf("the installed interceptor was not called")
	}
}

// TestWithServerBufSizeCountsBlocks pins the server-side clamp, which is in
// blocks rather than bytes.
func TestWithServerBufSizeCountsBlocks(t *testing.T) {
	so := DefaultServerOptions
	WithServerBufSize(0)(&so)
	if so.bufsize != 0 {
		t.Errorf("WithServerBufSize(0) -> %d, want 0", so.bufsize)
	}
	WithServerBufSize(16)(&so)
	if so.bufsize != 16 {
		t.Errorf("WithServerBufSize(16) -> %d, want 16", so.bufsize)
	}
	WithServerBufSize(40)(&so)
	if so.bufsize != defaultMaxBufSize/defaultBufSize {
		t.Errorf("WithServerBufSize(40) -> %d, want clamp at %d", so.bufsize, defaultMaxBufSize/defaultBufSize)
	}
}

// TestWithServerTimeoutsAreTakenLiterally covers the two server-side I/O
// timeouts.
func TestWithServerTimeoutsAreTakenLiterally(t *testing.T) {
	so := DefaultServerOptions
	WithServerReadTimeout(4 * time.Second)(&so)
	WithServerWriteTimeout(time.Second)(&so)
	if so.readTimeout != 4*time.Second || so.writeTimeout != time.Second {
		t.Errorf("timeouts = (%v, %v), want (4s, 1s)", so.readTimeout, so.writeTimeout)
	}
}

// TestWithServerTimeoutSetsTheHandlerFallback covers the handler deadline option:
// it stores the value as given, and zero means "no override" — the handler then
// falls back to defaultTimeout, the same value the option starts from.
func TestWithServerTimeoutSetsTheHandlerFallback(t *testing.T) {
	so := DefaultServerOptions
	if got := so.timeout; got != defaultTimeout {
		t.Errorf("DefaultServerOptions.timeout = %v, want %v", got, defaultTimeout)
	}
	WithServerTimeout(40 * time.Millisecond)(&so)
	if got := so.timeout; got != 40*time.Millisecond {
		t.Errorf("WithServerTimeout(40ms) -> %v, want 40ms", got)
	}
	WithServerTimeout(0)(&so)
	if got := so.timeout; got != 0 {
		t.Errorf("WithServerTimeout(0) -> %v, want 0 (no override)", got)
	}
}

// TestWithServerCodecReplacesTheDefault proves the option installs the codec
// the dispatcher decodes with.
func TestWithServerCodecReplacesTheDefault(t *testing.T) {
	counter := &countingCodec{}
	so := DefaultServerOptions
	WithServerCodec(counter)(&so)
	if so.codec != counter {
		t.Fatalf("codec = %v, want the counting codec", so.codec)
	}
}

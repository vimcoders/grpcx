package stats

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"runtime/metrics"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// resetCounters zeroes the process-wide counters, which the endpoint reports
// for the whole process and a test therefore cannot leave dirty.
func resetCounters() {
	calls.Store(0)
	inflight.Store(0)
	maxInflight.Store(0)
	handlerNs.Store(0)
	firstCallNs.Store(0)
	lastCallNs.Store(0)
	for i := range hist {
		hist[i].Store(0)
	}
}

// withEnabled sets the package-level switch for one test and restores both it
// and the counters afterwards.
func withEnabled(t *testing.T, on bool) {
	t.Helper()

	old := enabled
	enabled = on
	resetCounters()
	t.Cleanup(func() {
		enabled = old
		resetCounters()
	})
}

func TestEnabledMatchesEnv(t *testing.T) {
	if got, want := Enabled(), enabled; got != want {
		t.Fatalf("Enabled() = %v, want %v", got, want)
	}
}

// TestBeginEndDisabledIsNoop pins the dormancy contract: with the endpoint off,
// Begin reports zero and End records nothing.
func TestBeginEndDisabledIsNoop(t *testing.T) {
	withEnabled(t, false)

	if start := Begin(); start != 0 {
		t.Fatalf("Begin() while disabled = %d, want 0", start)
	}
	End(0)

	if got := calls.Load(); got != 0 {
		t.Fatalf("disabled End recorded %d calls", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Fatalf("disabled Begin left %d in flight", got)
	}
}

func TestBeginEndRecordsCall(t *testing.T) {
	withEnabled(t, true)

	start := Begin()
	if start <= 0 {
		t.Fatalf("Begin() = %d, want a wall clock timestamp", start)
	}
	if got := inflight.Load(); got != 1 {
		t.Fatalf("inflight after Begin = %d, want 1", got)
	}
	if got := maxInflight.Load(); got != 1 {
		t.Fatalf("max inflight after Begin = %d, want 1", got)
	}
	if got := firstCallNs.Load(); got != start {
		t.Fatalf("first call timestamp = %d, want %d", got, start)
	}

	// A measurable duration: two back-to-back clock reads can land in the same
	// tick and record zero.
	time.Sleep(time.Millisecond)
	End(start)

	if got := calls.Load(); got != 1 {
		t.Fatalf("calls after End = %d, want 1", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Fatalf("inflight after End = %d, want 0", got)
	}
	if got := handlerNs.Load(); got <= 0 {
		t.Fatalf("handler time = %d, want a positive duration", got)
	}
	if got := lastCallNs.Load(); got <= 0 {
		t.Fatalf("last call timestamp = %d, want a wall clock time", got)
	}

	var bucketed int64
	for i := range hist {
		bucketed += hist[i].Load()
	}
	if bucketed != 1 {
		t.Fatalf("histogram holds %d samples, want 1", bucketed)
	}
}

// TestBeginEndNestedTracksPeak checks the high water mark with overlapping
// calls.
func TestBeginEndNestedTracksPeak(t *testing.T) {
	withEnabled(t, true)

	outer := Begin()
	inner := Begin()
	End(inner)
	End(outer)

	if got := calls.Load(); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if got := maxInflight.Load(); got != 2 {
		t.Fatalf("max inflight = %d, want 2", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d, want 0 once both calls ended", got)
	}
}

// TestSlowCallLandsInLastBucket covers the clamp: a call slower than the
// tracked range must not index past the array.
func TestSlowCallLandsInLastBucket(t *testing.T) {
	withEnabled(t, true)

	start := time.Now().Add(-time.Second).UnixNano()
	End(start)

	if got := hist[histBuckets-1].Load(); got != 1 {
		t.Fatalf("slowest bucket holds %d samples, want 1", got)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
}

func TestUnaryInterceptorMeasuresCall(t *testing.T) {
	withEnabled(t, true)

	interceptor := UnaryInterceptor()
	handler := func(ctx context.Context, req any) (any, error) { return "reply", nil }

	reply, err := interceptor(t.Context(), "req", &grpc.UnaryServerInfo{FullMethod: "/api.EchoService/Echo"}, handler)
	if err != nil {
		t.Fatalf("interceptor = %v", err)
	}
	if reply != "reply" {
		t.Fatalf("interceptor reply = %v, want reply", reply)
	}
	if got := calls.Load(); got != 1 {
		t.Fatalf("calls = %d, want 1", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d, want 0", got)
	}
}

// TestUnaryInterceptorKeepsError checks that a failing handler still closes the
// measurement instead of leaving a call in flight.
func TestUnaryInterceptorKeepsError(t *testing.T) {
	withEnabled(t, true)

	handlerErr := errors.New("handler failed")
	handler := func(context.Context, any) (any, error) { return nil, handlerErr }

	reply, err := UnaryInterceptor()(t.Context(), nil, &grpc.UnaryServerInfo{}, handler)
	if !errors.Is(err, handlerErr) {
		t.Fatalf("interceptor = %v, want %v", err, handlerErr)
	}
	if reply != nil {
		t.Fatalf("interceptor reply = %v, want nil", reply)
	}
	if got := inflight.Load(); got != 0 {
		t.Fatalf("inflight = %d, want 0 after a failed handler", got)
	}
}

// TestPercentile pins the bucket arithmetic: p50, p90 and p99 pick the bucket
// that first reaches the requested share of the calls.
func TestPercentile(t *testing.T) {
	withEnabled(t, true)

	calls.Store(10)
	hist[0].Store(5)
	hist[5].Store(5)

	for _, tc := range []struct {
		p     float64
		want  float64
		label string
	}{
		{0.50, 1 * float64(histBucketNs) / 1000, "p50"},
		{0.90, 6 * float64(histBucketNs) / 1000, "p90"},
		{0.99, 6 * float64(histBucketNs) / 1000, "p99"},
	} {
		if got := percentile(tc.p); got != tc.want {
			t.Fatalf("percentile(%s) = %v, want %v", tc.label, got, tc.want)
		}
	}

	calls.Store(0)
	for i := range hist {
		hist[i].Store(0)
	}
	if got := percentile(0.5); got != 0 {
		t.Fatalf("percentile with no calls = %v, want 0", got)
	}
}

func TestAsUint64(t *testing.T) {
	var bad metrics.Value
	if got := asUint64(bad); got != 0 {
		t.Fatalf("asUint64(zero Value) = %d, want 0", got)
	}

	samples := []metrics.Sample{{Name: "/sched/gomaxprocs:threads"}}
	metrics.Read(samples)
	if got := asUint64(samples[0].Value); got == 0 {
		t.Fatalf("asUint64(gomaxprocs) = 0, want the live value")
	}
}

// TestSnapshotCollect covers every counter read and the runtime metrics block.
func TestSnapshotCollect(t *testing.T) {
	withEnabled(t, true)

	start := Begin()
	time.Sleep(time.Millisecond)
	End(start)

	var snap snapshot
	snap.collect()

	if snap.Calls != 1 {
		t.Fatalf("Calls = %d, want 1", snap.Calls)
	}
	if snap.HandlerNsSum <= 0 || snap.HandlerUsAvg <= 0 {
		t.Fatalf("handler totals = %d ns / %v us", snap.HandlerNsSum, snap.HandlerUsAvg)
	}
	if snap.MaxInflight < 1 {
		t.Fatalf("MaxInflight = %d, want at least 1", snap.MaxInflight)
	}
	if snap.UptimeS <= 0 {
		t.Fatalf("UptimeS = %v, want a positive uptime", snap.UptimeS)
	}
	if snap.Gomaxprocs <= 0 {
		t.Fatalf("Gomaxprocs = %d, want a positive value", snap.Gomaxprocs)
	}
	if snap.Goroutines == 0 {
		t.Fatalf("Goroutines = 0, want the live count")
	}
	if snap.AllocObjects == 0 && snap.GCCycles == 0 && snap.HeapObjectsBytes == 0 {
		t.Fatalf("runtime metrics were not filled: %+v", snap)
	}
}

// TestStatsHandler checks the response the endpoint serves: JSON content type,
// indented body, and the snapshot fields present.
func TestStatsHandler(t *testing.T) {
	withEnabled(t, true)

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/stats", nil)
	statsHandler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Fatalf("content type = %q, want application/json", got)
	}

	body, err := io.ReadAll(rec.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	var got map[string]any
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("body is not JSON: %v\n%s", err, body)
	}
	for _, key := range []string{"uptime_s", "calls", "gomaxprocs", "handler_ns_sum"} {
		if _, ok := got[key]; !ok {
			t.Fatalf("snapshot is missing %q: %s", key, body)
		}
	}
}

// TestServeDormantWithoutEnv makes sure the endpoint stays off unless the
// environment asks for it: Serve must return instead of binding a port.
func TestServeDormantWithoutEnv(t *testing.T) {
	t.Setenv(envAddr, "")

	done := make(chan struct{})
	go func() {
		Serve()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("Serve() blocked with %s unset", envAddr)
	}
}

// TestProcAndCgroupHelpers calls the OS specific readers; outside Linux they
// return zeros, and the test only requires that they do not fail loudly.
func TestProcAndCgroupHelpers(t *testing.T) {
	if got := cgroupCPUUsec(); got > 1<<62 {
		t.Fatalf("cgroup cpu time = %d, implausible", got)
	}
	_, _, _, _ = procIO()
}

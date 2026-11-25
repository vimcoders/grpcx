package stats

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"runtime/metrics"
	"testing"
	"time"

	"google.golang.org/grpc"
)

// resetMetrics clears every counter so a test can assert absolute values.
func resetMetrics() {
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

// withEnabled runs the test body with the package switch set, restoring the
// original value and clearing the counters afterwards.
func withEnabled(t *testing.T, on bool) {
	t.Helper()
	previous := enabled
	enabled = on
	resetMetrics()
	t.Cleanup(func() {
		enabled = previous
		resetMetrics()
	})
}

// TestEnableFollowsThePackageSwitch covers the accessor the hot path checks.
func TestEnableFollowsThePackageSwitch(t *testing.T) {
	previous := enabled
	t.Cleanup(func() { enabled = previous })

	enabled = false
	if Enabled() {
		t.Error("Enabled() = true while the switch is off")
	}
	enabled = true
	if !Enabled() {
		t.Error("Enabled() = false while the switch is on")
	}
}

// TestBeginReturnsZeroWhenDisabled covers the dormant path: no timestamp, so End
// has nothing to record.
func TestBeginReturnsZeroWhenDisabled(t *testing.T) {
	withEnabled(t, false)
	if got := Begin(); got != 0 {
		t.Errorf("Begin() = %d, want 0 while disabled", got)
	}
	End(0)
	if got := calls.Load(); got != 0 {
		t.Errorf("calls = %d, want 0 after End(0)", got)
	}
}

// TestBeginAndEndRecordOneCall covers the accounting: one call is counted, timed
// and bucketed, and its inflight slot is released.
func TestBeginAndEndRecordOneCall(t *testing.T) {
	withEnabled(t, true)

	start := Begin()
	if start == 0 {
		t.Fatal("Begin() = 0 while enabled")
	}
	if got := inflight.Load(); got != 1 {
		t.Errorf("inflight = %d, want 1 during a call", got)
	}
	if got := maxInflight.Load(); got < 1 {
		t.Errorf("maxInflight = %d, want at least 1", got)
	}
	if firstCallNs.Load() != start {
		t.Error("the first call timestamp was not recorded")
	}

	time.Sleep(200 * time.Microsecond)
	End(start)
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Errorf("inflight = %d, want 0 after the call", got)
	}
	if got := handlerNs.Load(); got <= 0 {
		t.Errorf("handlerNs = %d, want a positive duration", got)
	}
	if got := lastCallNs.Load(); got == 0 {
		t.Error("the last call timestamp was not recorded")
	}
}

// TestEndIgnoresAZeroStart covers the disabled pairing.
func TestEndIgnoresAZeroStart(t *testing.T) {
	withEnabled(t, true)
	End(0)
	if got := calls.Load(); got != 0 {
		t.Errorf("calls = %d, want 0", got)
	}
	if got := inflight.Load(); got != 0 {
		t.Errorf("inflight = %d, want 0", got)
	}
}

// TestPercentileWalksTheHistogram covers the percentiles, including the bucket
// boundary and the empty case.
func TestPercentileWalksTheHistogram(t *testing.T) {
	withEnabled(t, true)

	if got := percentile(0.5); got != 0 {
		t.Errorf("percentile(0.5) with no calls = %v, want 0", got)
	}

	calls.Store(100)
	hist[10].Store(90) // 90 calls in the 11th bucket, i.e. 1.1µs
	hist[20].Store(10) // the remaining 10 land at 2.1µs

	if got, want := percentile(0.50), 1.1; got != want {
		t.Errorf("p50 = %v, want %v", got, want)
	}
	if got, want := percentile(0.90), 1.1; got != want {
		t.Errorf("p90 = %v, want %v", got, want)
	}
	if got, want := percentile(0.99), 2.1; got != want {
		t.Errorf("p99 = %v, want %v", got, want)
	}
}

// TestPercentileFallsBackToTheLastBucket covers a histogram whose counts do not
// add up to the call count: the walk runs out and reports the cap.
func TestPercentileFallsBackToTheLastBucket(t *testing.T) {
	withEnabled(t, true)
	calls.Store(10)

	if got, want := percentile(0.99), float64(histBuckets)*histBucketNs/1000; got != want {
		t.Errorf("p99 over an empty histogram = %v, want the cap %v", got, want)
	}
}

// TestAsUint64ConvertsBothKinds covers the two metric kinds the snapshot reads.
func TestAsUint64ConvertsBothKinds(t *testing.T) {
	samples := []metrics.Sample{
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/cpu/classes/total:cpu-seconds"},
		{Name: "/not/a/real/metric"},
	}
	metrics.Read(samples)

	if got, want := asUint64(samples[0].Value), samples[0].Value.Uint64(); got != want {
		t.Errorf("asUint64(uint64) = %d, want %d", got, want)
	}
	if got, want := asUint64(samples[1].Value), uint64(samples[1].Value.Float64()); got != want {
		t.Errorf("asUint64(float64) = %d, want %d", got, want)
	}
	if got := asUint64(samples[2].Value); got != 0 {
		t.Errorf("asUint64(unsupported) = %d, want 0", got)
	}
}

// TestSnapshotCollectsEveryCounter covers collect: the recorded call shows up in
// every derived field, and the runtime metrics are read.
func TestSnapshotCollectsEveryCounter(t *testing.T) {
	withEnabled(t, true)
	start := Begin()
	time.Sleep(200 * time.Microsecond)
	End(start)

	var snap snapshot
	snap.collect()

	if snap.Calls != 1 {
		t.Errorf("Calls = %d, want 1", snap.Calls)
	}
	if snap.HandlerUsAvg <= 0 {
		t.Errorf("HandlerUsAvg = %v, want a positive average", snap.HandlerUsAvg)
	}
	if snap.HandlerUsP50 <= 0 || snap.HandlerUsP99 <= 0 {
		t.Errorf("percentiles = (%v, %v), want positive values", snap.HandlerUsP50, snap.HandlerUsP99)
	}
	if snap.Gomaxprocs <= 0 {
		t.Errorf("Gomaxprocs = %d, want a positive value", snap.Gomaxprocs)
	}
	if snap.Goroutines == 0 {
		t.Error("Goroutines = 0, want the runtime metric read")
	}
}

// TestSnapshotReportsWallTimePerCall covers the span fields, which need at least
// two calls to be meaningful.
func TestSnapshotReportsWallTimePerCall(t *testing.T) {
	withEnabled(t, true)
	for range 2 {
		start := Begin()
		time.Sleep(100 * time.Microsecond)
		End(start)
	}

	var snap snapshot
	snap.collect()
	if snap.SpanNs <= 0 {
		t.Errorf("SpanNs = %d, want a positive span", snap.SpanNs)
	}
	if snap.WallNsPerCall <= 0 {
		t.Errorf("WallNsPerCall = %v, want a positive value with two calls", snap.WallNsPerCall)
	}
}

// TestStatsHandlerServesTheSnapshot covers the endpoint: /stats answers JSON with
// every field the snapshot has.
func TestStatsHandlerServesTheSnapshot(t *testing.T) {
	withEnabled(t, true)
	End(Begin())

	recorder := httptest.NewRecorder()
	statsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/stats", nil))

	if recorder.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", recorder.Code)
	}
	if got := recorder.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}

	var body map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		t.Fatalf("decode body: %v", err)
	}
	for _, key := range []string{"calls", "handler_us_p99", "max_inflight", "gomaxprocs", "uptime_s"} {
		if _, ok := body[key]; !ok {
			t.Errorf("body is missing %q", key)
		}
	}
}

// TestStatsHandlerRejectsOtherPaths covers the mux: only /stats is served.
func TestStatsHandlerRejectsOtherPaths(t *testing.T) {
	recorder := httptest.NewRecorder()
	statsHandler().ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/metrics", nil))
	if recorder.Code != http.StatusNotFound {
		t.Errorf("status = %d, want 404", recorder.Code)
	}
}

// TestServeDoesNothingWithoutAnAddress covers the dormant start.
func TestServeDoesNothingWithoutAnAddress(t *testing.T) {
	t.Setenv(envAddr, "")
	Serve()
}

// TestUnaryInterceptorRecordsTheCall covers the wrapper: the handler answer is
// returned unchanged and the call is counted.
func TestUnaryInterceptorRecordsTheCall(t *testing.T) {
	withEnabled(t, true)

	reply, err := UnaryInterceptor()(context.Background(), "req", &grpc.UnaryServerInfo{}, func(context.Context, any) (any, error) {
		return "reply", nil
	})
	if err != nil || reply != "reply" {
		t.Fatalf("interceptor chain = (%v, %v), want (reply, nil)", reply, err)
	}
	if got := calls.Load(); got != 1 {
		t.Errorf("calls = %d, want 1", got)
	}
}

// TestProcCountersAreReadable covers both /proc readers on the host they run on:
// they must not panic, and the values must never go backwards.
func TestProcCountersAreReadable(t *testing.T) {
	cpu := cgroupCPUUsec()
	if runtime.GOOS == "linux" {
		if cpu == 0 {
			t.Log("cgroup v2 cpu.stat is not readable here; the zero path is exercised instead")
		}
	} else if cpu != 0 {
		t.Errorf("cgroupCPUUsec = %d on %s, want 0 without cgroup v2", cpu, runtime.GOOS)
	}

	first := [4]uint64{}
	first[0], first[1], first[2], first[3] = procIO()
	second := [4]uint64{}
	second[0], second[1], second[2], second[3] = procIO()
	for i := range first {
		if second[i] < first[i] {
			t.Errorf("procIO counter %d went backwards: %d -> %d", i, first[i], second[i])
		}
	}
	if runtime.GOOS != "linux" {
		for i, v := range first {
			if v != 0 {
				t.Errorf("procIO counter %d = %d on %s, want 0 without /proc/self/io", i, v, runtime.GOOS)
			}
		}
	}
}

// TestEnvAddrIsTheDocumentedName covers the constant the whole package reads.
func TestEnvAddrIsTheDocumentedName(t *testing.T) {
	if envAddr != "GRPCX_STATS_ADDR" {
		t.Errorf("envAddr = %q, want GRPCX_STATS_ADDR", envAddr)
	}
}

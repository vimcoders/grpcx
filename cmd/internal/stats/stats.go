// Package stats exposes process-level metrics for the demo servers in cmd/grpc
// and cmd/grpcx, so that benchmark runs can look at the server side and not
// only at what the client measures.
//
// It stays dormant unless GRPCX_STATS_ADDR is set, which keeps the demo
// servers byte-for-byte as fast as before in every other situation.
package stats

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"runtime"
	"runtime/metrics"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"google.golang.org/grpc"
)

// envAddr names the address the metrics endpoint listens on, e.g. ":9091".
const envAddr = "GRPCX_STATS_ADDR"

// histBucketNs is the handler duration histogram resolution, and histBuckets
// caps the tracked range at 128*100ns; slower calls land in the last bucket.
const (
	histBucketNs = 100
	histBuckets  = 128
)

var (
	startedAt = time.Now()

	// enabled is read once: the endpoint is configured through the container
	// environment, and caching it keeps the enabled check out of the hot path.
	enabled = os.Getenv(envAddr) != ""

	calls       atomic.Int64
	inflight    atomic.Int64
	maxInflight atomic.Int64
	handlerNs   atomic.Int64
	firstCallNs atomic.Int64
	lastCallNs  atomic.Int64
	hist        [histBuckets]atomic.Int64
)

// Enabled reports whether the metrics endpoint was requested.
func Enabled() bool {
	return enabled
}

// Begin timestamps the start of a call. Pair it with End.
func Begin() int64 {
	if !Enabled() {
		return 0
	}
	now := time.Now().UnixNano()
	n := inflight.Add(1)
	for {
		seen := maxInflight.Load()
		if n <= seen || maxInflight.CompareAndSwap(seen, n) {
			break
		}
	}
	firstCallNs.CompareAndSwap(0, now)
	return now
}

// End records the duration of a call started by Begin.
func End(start int64) {
	if start == 0 {
		return
	}
	now := time.Now().UnixNano()
	d := now - start
	handlerNs.Add(d)
	idx := int(d / histBucketNs)
	if idx >= histBuckets {
		idx = histBuckets - 1
	}
	hist[idx].Add(1)
	lastCallNs.Store(now)
	calls.Add(1)
	inflight.Add(-1)
}

// UnaryInterceptor measures every unary call. It is only installed when the
// endpoint is enabled, so that the benchmark path stays untouched otherwise.
func UnaryInterceptor() grpc.UnaryServerInterceptor {
	return func(ctx context.Context, req any, info *grpc.UnaryServerInfo, handler grpc.UnaryHandler) (any, error) {
		start := Begin()
		resp, err := handler(ctx, req)
		End(start)
		return resp, err
	}
}

type snapshot struct {
	UptimeS          float64 `json:"uptime_s"`
	Calls            int64   `json:"calls"`
	Inflight         int64   `json:"inflight"`
	MaxInflight      int64   `json:"max_inflight"`
	HandlerNsSum     int64   `json:"handler_ns_sum"`
	HandlerUsAvg     float64 `json:"handler_us_avg"`
	HandlerUsP50     float64 `json:"handler_us_p50"`
	HandlerUsP90     float64 `json:"handler_us_p90"`
	HandlerUsP99     float64 `json:"handler_us_p99"`
	SpanNs           int64   `json:"span_ns"`
	WallNsPerCall    float64 `json:"wall_ns_per_call"`
	AllocBytes       uint64  `json:"alloc_bytes"`
	AllocObjects     uint64  `json:"alloc_objects"`
	GCCycles         uint64  `json:"gc_cycles"`
	Goroutines       uint64  `json:"goroutines"`
	HeapObjectsBytes uint64  `json:"heap_objects_bytes"`
	CPUUsec          uint64  `json:"cpu_usec"`
	Rchar            uint64  `json:"rchar"`
	Wchar            uint64  `json:"wchar"`
	Syscr            uint64  `json:"syscr"`
	Syscw            uint64  `json:"syscw"`
	Gomaxprocs       int     `json:"gomaxprocs"`
}

func (s *snapshot) collect() {
	s.UptimeS = time.Since(startedAt).Seconds()
	s.Calls = calls.Load()
	s.Inflight = inflight.Load()
	s.MaxInflight = maxInflight.Load()
	s.HandlerNsSum = handlerNs.Load()
	if s.Calls > 0 {
		s.HandlerUsAvg = float64(s.HandlerNsSum) / float64(s.Calls) / 1000
	}
	s.HandlerUsP50 = percentile(0.50)
	s.HandlerUsP90 = percentile(0.90)
	s.HandlerUsP99 = percentile(0.99)
	if first, last := firstCallNs.Load(), lastCallNs.Load(); first > 0 && last > first {
		s.SpanNs = last - first
		if s.Calls > 1 {
			s.WallNsPerCall = float64(s.SpanNs) / float64(s.Calls-1)
		}
	}
	s.Gomaxprocs = runtime.GOMAXPROCS(0)

	samples := []metrics.Sample{
		{Name: "/gc/heap/allocs:bytes"},
		{Name: "/gc/heap/allocs:objects"},
		{Name: "/gc/cycles/total:gc-cycles"},
		{Name: "/sched/goroutines:goroutines"},
		{Name: "/memory/classes/heap/objects:bytes"},
	}
	metrics.Read(samples)
	for _, sample := range samples {
		value := asUint64(sample.Value)
		switch sample.Name {
		case "/gc/heap/allocs:bytes":
			s.AllocBytes = value
		case "/gc/heap/allocs:objects":
			s.AllocObjects = value
		case "/gc/cycles/total:gc-cycles":
			s.GCCycles = value
		case "/sched/goroutines:goroutines":
			s.Goroutines = value
		case "/memory/classes/heap/objects:bytes":
			s.HeapObjectsBytes = value
		}
	}

	s.CPUUsec = cgroupCPUUsec()
	s.Rchar, s.Wchar, s.Syscr, s.Syscw = procIO()
}

func percentile(p float64) float64 {
	total := calls.Load()
	if total == 0 {
		return 0
	}
	limit := max(int64(float64(total)*p), 1)
	var seen int64
	for i := range hist {
		seen += hist[i].Load()
		if seen >= limit {
			return float64(i+1) * histBucketNs / 1000
		}
	}
	return float64(histBuckets) * histBucketNs / 1000
}

func asUint64(v metrics.Value) uint64 {
	switch v.Kind() {
	case metrics.KindUint64:
		return v.Uint64()
	case metrics.KindFloat64:
		return uint64(v.Float64())
	default:
		return 0
	}
}

// cgroupCPUUsec reads container CPU time from the cgroup v2 counters.
func cgroupCPUUsec() uint64 {
	file, err := os.Open("/sys/fs/cgroup/cpu.stat")
	if err != nil {
		return 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 || fields[0] != "usage_usec" {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			return 0
		}
		return value
	}
	if err := scanner.Err(); err != nil {
		log.Printf("stats: read cpu.stat: %v", err)
	}
	return 0
}

// procIO reads the process I/O counters maintained by the kernel.
func procIO() (rchar, wchar, syscr, syscw uint64) {
	file, err := os.Open("/proc/self/io")
	if err != nil {
		return 0, 0, 0, 0
	}
	defer file.Close()

	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) != 2 {
			continue
		}
		value, err := strconv.ParseUint(fields[1], 10, 64)
		if err != nil {
			continue
		}
		switch strings.TrimSuffix(fields[0], ":") {
		case "rchar":
			rchar = value
		case "wchar":
			wchar = value
		case "syscr":
			syscr = value
		case "syscw":
			syscw = value
		}
	}
	if err := scanner.Err(); err != nil {
		log.Printf("stats: read self io: %v", err)
	}
	return rchar, wchar, syscr, syscw
}

// Serve starts the metrics endpoint when GRPCX_STATS_ADDR is set.
func Serve() {
	addr := os.Getenv(envAddr)
	if addr == "" {
		return
	}

	mux := http.NewServeMux()
	mux.HandleFunc("/stats", func(w http.ResponseWriter, _ *http.Request) {
		var snap snapshot
		snap.collect()
		w.Header().Set("Content-Type", "application/json")
		encoder := json.NewEncoder(w)
		encoder.SetIndent("", "  ")
		_ = encoder.Encode(&snap)
	})

	server := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		if err := server.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("stats: endpoint failed: %v", err)
		}
	}()
}

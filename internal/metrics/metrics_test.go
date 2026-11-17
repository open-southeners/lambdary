package metrics

import (
	"math"
	"sync"
	"testing"
	"time"
)

var t0 = time.Date(2026, 11, 16, 12, 0, 0, 0, time.UTC)

func ms(n int) time.Duration { return time.Duration(n) * time.Millisecond }

// fakeClock is a settable clock.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *fakeClock) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) set(t time.Time) {
	c.mu.Lock()
	c.t = t
	c.mu.Unlock()
}

func newRec(now time.Time) (*Recorder, *fakeClock) {
	c := &fakeClock{t: now}
	return NewWithClock(c.now), c
}

func values(dps []Datapoint) []float64 {
	out := make([]float64, len(dps))
	for i, d := range dps {
		out[i] = d.Value
	}
	return out
}

func equal(a, b []float64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if math.Abs(a[i]-b[i]) > 1e-9 {
			return false
		}
	}
	return true
}

// seed records four invocations of "ping" in one minute: 10, 20, 30 and 40
// ms, the 30 ms one failing, under resources ping:1 and ping:live.
func seed(r *Recorder) {
	for i, d := range []int{10, 20, 30, 40} {
		r.Record(Invocation{
			Function:  "ping",
			Resources: []string{"ping:1", "ping:live"},
			Start:     t0.Add(time.Duration(i) * time.Second),
			Duration:  ms(d),
			Error:     d == 30,
		})
	}
}

func TestStats(t *testing.T) {
	r, _ := newRec(t0.Add(time.Minute))
	seed(r)
	base := Query{Function: "ping", Start: t0, End: t0.Add(time.Minute), Period: time.Minute}

	tests := []struct {
		metric, stat string
		want         float64
	}{
		{Invocations, "Sum", 4},
		{Invocations, "SampleCount", 4},
		{Invocations, "Average", 1},
		{Invocations, "Minimum", 1},
		{Invocations, "Maximum", 1},
		{Errors, "Sum", 1},
		{Errors, "SampleCount", 4},
		{Errors, "Average", 0.25},
		{Errors, "Minimum", 0},
		{Errors, "Maximum", 1},
		{Errors, "p50", 0},
		{Errors, "p90", 1},
		{Throttles, "Sum", 0},
		{Throttles, "SampleCount", 4},
		{Throttles, "Maximum", 0},
		{Duration, "Sum", 100},
		{Duration, "SampleCount", 4},
		{Duration, "Average", 25},
		{Duration, "Minimum", 10},
		{Duration, "Maximum", 40},
		{Duration, "p50", 20},
		{Duration, "p75", 30},
		{Duration, "p99", 40},
		{Duration, "p99.9", 40},
		{Duration, "p0", 10},
		{Duration, "p100", 40},
	}
	for _, tt := range tests {
		t.Run(tt.metric+"/"+tt.stat, func(t *testing.T) {
			q := base
			q.Metric, q.Stat = tt.metric, tt.stat
			got := r.Query(q)
			if len(got) != 1 {
				t.Fatalf("got %d datapoints, want 1: %v", len(got), got)
			}
			if got[0].Value != tt.want {
				t.Errorf("got %v, want %v", got[0].Value, tt.want)
			}
			if !got[0].Timestamp.Equal(t0) {
				t.Errorf("timestamp = %v, want %v", got[0].Timestamp, t0)
			}
		})
	}
}

func TestQueryRejectsBadInput(t *testing.T) {
	r, _ := newRec(t0)
	seed(r)
	base := Query{Function: "ping", Start: t0, End: t0.Add(time.Minute), Metric: Duration, Stat: "Sum"}
	for name, mut := range map[string]func(*Query){
		"metric":     func(q *Query) { q.Metric = "ConcurrentExecutions" },
		"stat":       func(q *Query) { q.Stat = "Median" },
		"percentile": func(q *Query) { q.Stat = "p101" },
		"empty stat": func(q *Query) { q.Stat = "" },
		"function":   func(q *Query) { q.Function = "other" },
		"resource":   func(q *Query) { q.Resource = "ping:9" },
	} {
		q := base
		mut(&q)
		if got := r.Query(q); got != nil {
			t.Errorf("%s: got %v, want nil", name, got)
		}
	}
}

func TestResourceSeries(t *testing.T) {
	r, _ := newRec(t0.Add(time.Minute))
	r.Record(Invocation{Function: "ping", Resources: []string{"ping:1", "ping:1", ""}, Start: t0, Duration: ms(5)})
	r.Record(Invocation{Function: "ping", Resources: []string{"ping:2"}, Start: t0, Duration: ms(7)})
	q := Query{Metric: Invocations, Stat: "Sum", Function: "ping", Start: t0, End: t0.Add(time.Minute)}
	for res, want := range map[string]float64{"": 2, "ping:1": 1, "ping:2": 1} {
		q.Resource = res
		got := r.Query(q)
		if len(got) != 1 || got[0].Value != want {
			t.Errorf("resource %q: got %v, want %v", res, got, want)
		}
	}
}

func TestPeriodAggregation(t *testing.T) {
	r, _ := newRec(t0.Add(time.Hour))
	// Minutes 0, 1, 2 and 7 after an unaligned start.
	for _, m := range []int{0, 1, 2, 7} {
		r.Record(Invocation{Function: "f", Start: t0.Add(time.Duration(m)*time.Minute + 30*time.Second), Duration: ms(10 * (m + 1))})
	}
	q := Query{
		Metric: Invocations, Stat: "Sum", Function: "f",
		Start: t0.Add(15 * time.Second), End: t0.Add(30 * time.Minute), Period: 5 * time.Minute,
	}
	got := r.Query(q)
	if !equal(values(got), []float64{3, 1}) {
		t.Fatalf("values = %v, want [3 1]", values(got))
	}
	if !got[0].Timestamp.Equal(t0) || !got[1].Timestamp.Equal(t0.Add(5*time.Minute)) {
		t.Errorf("timestamps = %v, %v; want aligned to the minute-rounded start", got[0].Timestamp, got[1].Timestamp)
	}

	// Empty periods are omitted: 5-minute periods 0 and 1 only, none after.
	q.End = t0.Add(5 * time.Minute)
	if got := r.Query(q); len(got) != 1 {
		t.Errorf("End is exclusive: got %v", got)
	}
	q.Start = t0.Add(10 * time.Minute)
	q.End = t0.Add(20 * time.Minute)
	if got := r.Query(q); len(got) != 0 {
		t.Errorf("empty range: got %v", got)
	}

	// Duration percentiles merge samples across the buckets of a period.
	q = Query{Metric: Duration, Stat: "Maximum", Function: "f", Start: t0, End: t0.Add(5 * time.Minute), Period: 5 * time.Minute}
	if got := r.Query(q); len(got) != 1 || got[0].Value != 30 {
		t.Errorf("max across buckets = %v, want 30", got)
	}
	q.Stat = "Average"
	if got := r.Query(q); len(got) != 1 || got[0].Value != 20 {
		t.Errorf("avg across buckets = %v, want 20", got)
	}
	// Default period is one minute.
	q.Period, q.End, q.Stat = 0, t0.Add(10*time.Minute), "Sum"
	if got := r.Query(q); len(got) != 4 {
		t.Errorf("default period: got %d datapoints, want 4", len(got))
	}
}

func TestErrorsAndThrottlesReporting(t *testing.T) {
	r, _ := newRec(t0.Add(time.Hour))
	r.Record(Invocation{Function: "f", Start: t0, Duration: ms(1)})                  // minute 0: healthy
	r.Record(Invocation{Function: "f", Start: t0.Add(time.Minute), Throttled: true}) // minute 1: only throttled
	r.Record(Invocation{Function: "f", Start: t0.Add(2 * time.Minute), Duration: ms(1), Error: true})
	r.Record(Invocation{Function: "f", Start: t0.Add(2 * time.Minute), Throttled: true})

	q := Query{Function: "f", Stat: "Sum", Start: t0, End: t0.Add(3 * time.Minute)}
	for metric, want := range map[string][]float64{
		Invocations: {1, 1},    // throttled invocations never ran
		Errors:      {0, 1},    // healthy period reports 0
		Throttles:   {0, 1, 1}, // reported whenever something happened
		Duration:    {1, 1},
	} {
		q.Metric = metric
		if got := values(r.Query(q)); !equal(got, want) {
			t.Errorf("%s = %v, want %v", metric, got, want)
		}
	}
}

func TestReservoirCap(t *testing.T) {
	r, _ := newRec(t0.Add(time.Minute))
	const n = 5000
	for i := 1; i <= n; i++ {
		r.Record(Invocation{Function: "f", Start: t0, Duration: ms(i)})
	}
	r.mu.Lock()
	got := len(r.series[seriesKey{function: "f"}][t0.Unix()].samples)
	r.mu.Unlock()
	if got != maxSamples {
		t.Fatalf("samples = %d, want %d", got, maxSamples)
	}
	q := Query{Metric: Duration, Function: "f", Start: t0, End: t0.Add(time.Minute), Period: time.Minute}
	for stat, want := range map[string]float64{"Sum": n * (n + 1) / 2, "SampleCount": n, "Minimum": 1, "Maximum": n} {
		q.Stat = stat
		if dp := r.Query(q); len(dp) != 1 || dp[0].Value != want {
			t.Errorf("%s = %v, want %v (exact despite sampling)", stat, dp, want)
		}
	}
	q.Stat = "p50"
	if dp := r.Query(q); len(dp) != 1 || dp[0].Value < 2000 || dp[0].Value > 3000 {
		t.Errorf("sampled p50 = %v, want near 2500", dp)
	}
}

func TestRetention(t *testing.T) {
	r, clock := newRec(t0)
	r.Record(Invocation{Function: "f", Resources: []string{"f:1"}, Start: t0, Duration: ms(1)})

	// An invocation already past retention is discarded outright.
	r.Record(Invocation{Function: "old", Start: t0.Add(-Retention - time.Hour), Duration: ms(1)})
	r.mu.Lock()
	_, hasOld := r.series[seriesKey{function: "old"}]
	r.mu.Unlock()
	if hasOld {
		t.Error("expired invocation was stored")
	}

	// Later, a write prunes the aged bucket and its empty series.
	clock.set(t0.Add(Retention + 2*time.Minute))
	r.Record(Invocation{Function: "g", Start: clock.now(), Duration: ms(1)})
	r.mu.Lock()
	_, hasF := r.series[seriesKey{function: "f"}]
	_, hasRes := r.series[seriesKey{function: "f", resource: "f:1"}]
	_, hasG := r.series[seriesKey{function: "g"}]
	r.mu.Unlock()
	if hasF || hasRes || !hasG {
		t.Errorf("after prune: f=%v f:1=%v g=%v, want false false true", hasF, hasRes, hasG)
	}
}

func TestSnapshot(t *testing.T) {
	r, clock := newRec(t0.Add(30 * time.Minute))
	seed(r) // minute t0 -> 30 minutes old
	r.Record(Invocation{Function: "a", Start: t0.Add(25 * time.Minute), Duration: ms(9), Throttled: true})
	r.Record(Invocation{Function: "ping", Resources: []string{"ping:live"}, Start: t0.Add(29 * time.Minute), Duration: ms(100), Error: true})

	snap := r.Snapshot(time.Hour)
	if len(snap) != 2 || snap[0].Name != "a" || snap[1].Name != "ping" {
		t.Fatalf("snapshot = %+v", snap)
	}
	if snap[0].Total.Throttles != 1 || snap[0].Total.Invocations != 0 {
		t.Errorf("a total = %+v", snap[0].Total)
	}
	p := snap[1]
	if p.Total.Invocations != 5 || p.Total.Errors != 2 || p.Total.DurationMS.Max != 100 ||
		p.Total.DurationMS.P50 != 30 || p.Total.DurationMS.P95 != 100 {
		t.Errorf("ping total = %+v", p.Total)
	}
	if len(p.Resources) != 2 || p.Resources[0].Resource != "ping:1" || p.Resources[1].Resource != "ping:live" ||
		p.Resources[0].Invocations != 4 || p.Resources[1].Invocations != 5 {
		t.Errorf("ping resources = %+v", p.Resources)
	}

	// A narrow window drops the older minute.
	snap = r.Snapshot(4 * time.Minute)
	if len(snap) != 1 || snap[0].Total.Invocations != 1 {
		t.Errorf("4m snapshot = %+v", snap)
	}
	if r.Snapshot(0) != nil {
		t.Error("zero window should be empty")
	}
	_ = clock
}

func TestParseStat(t *testing.T) {
	for _, s := range []string{"Sum", "SampleCount", "Average", "Minimum", "Maximum", "p50", "p99.9", "p0", "p100"} {
		if _, err := ParseStat(s); err != nil {
			t.Errorf("ParseStat(%q): %v", s, err)
		}
	}
	for _, s := range []string{"", "p", "pabc", "p-1", "p100.1", "sum", "TM(1:2)", "pNaN"} {
		if _, err := ParseStat(s); err == nil {
			t.Errorf("ParseStat(%q) succeeded", s)
		}
	}
}

func TestConcurrent(t *testing.T) {
	r := New()
	now := time.Now()
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 500; i++ {
				r.Record(Invocation{Function: "f", Resources: []string{"f:1"}, Start: time.Now(), Duration: ms(i), Error: i%10 == 0})
				r.Query(Query{Metric: Duration, Function: "f", Stat: "p95", Start: now.Add(-time.Hour), End: now.Add(time.Hour)})
				r.Snapshot(time.Hour)
			}
		}()
	}
	wg.Wait()
	got := r.Query(Query{Metric: Invocations, Function: "f", Stat: "Sum", Start: now.Add(-time.Hour), End: now.Add(time.Hour), Period: 2 * time.Hour})
	var total float64
	for _, d := range got {
		total += d.Value
	}
	if total != 4000 {
		t.Errorf("total = %v, want 4000", total)
	}
}

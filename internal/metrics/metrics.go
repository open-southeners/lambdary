// Package metrics keeps Lambdary's local equivalent of the AWS/Lambda
// CloudWatch metrics: invocation, error, throttle and duration figures for
// every function, queryable the way CloudWatch's GetMetricData does and
// summarised for Lambdary's own JSON endpoint. Everything lives in memory
// and is lost when the dev server stops, like the real service's metrics of
// a function that was never deployed.
//
// Each invocation is recorded once and counts under several series: the
// FunctionName-only series, plus one series per Resource dimension value
// (for example "ping:3" and "ping:live"). A series stores one bucket per UTC
// minute with exact counts, an exact duration sum, minimum and maximum, and
// at most 1,000 duration samples (reservoir sampling) for percentiles.
// Buckets older than 14 days are dropped lazily while recording.
//
// Throttled invocations follow CloudWatch: the function never ran, so they
// count only under Throttles, not under Invocations, Errors or Duration.
package metrics

import (
	"math/rand/v2"
	"sort"
	"sync"
	"time"
)

// Metric names, as they appear in a CloudWatch query's MetricName.
const (
	// Invocations counts invocations that reached the function.
	Invocations = "Invocations"
	// Errors counts invocations that ended in a function or upstream error.
	Errors = "Errors"
	// Throttles counts invocations rejected because the function was busy.
	Throttles = "Throttles"
	// Duration measures invocation duration in milliseconds.
	Duration = "Duration"
)

const (
	// Retention is how long buckets are kept, like CloudWatch's one-minute
	// resolution retention of 15 days, rounded down.
	Retention = 14 * 24 * time.Hour

	// maxSamples caps the duration samples kept per bucket.
	maxSamples = 1000

	// defaultPeriod is used when a Query leaves Period unset.
	defaultPeriod = time.Minute
)

// Invocation is one finished invocation to record.
type Invocation struct {
	// Function is the Lambdary function name (the FunctionName dimension).
	Function string
	// Resources are the Resource dimension values the invocation also counts
	// under, such as "ping:3" and "ping:live". The invocation always counts
	// under the FunctionName-only series as well. Duplicates and empty
	// strings are ignored.
	Resources []string
	// Start is when the invocation began; it picks the minute bucket.
	Start time.Time
	// Duration is how long the invocation took. Ignored when Throttled.
	Duration time.Duration
	// Error marks a function error or an upstream/timeout failure.
	Error bool
	// Throttled marks an invocation that was rejected without running.
	Throttled bool
}

// seriesKey identifies one series. Resource is "" for the function-only one.
type seriesKey struct {
	function string
	resource string
}

// bucket aggregates one series over one UTC minute.
type bucket struct {
	count     int64 // invocations that ran
	errors    int64 // of those, how many failed
	throttles int64 // invocations rejected without running

	durSum  float64 // exact sum of durations in ms
	durMin  float64
	durMax  float64
	samples []float64 // at most maxSamples, uniformly sampled
}

// add folds one invocation into the bucket.
func (b *bucket) add(inv Invocation) {
	if inv.Throttled {
		b.throttles++
		return
	}
	ms := durationMS(inv.Duration)
	b.count++
	if inv.Error {
		b.errors++
	}
	b.durSum += ms
	if b.count == 1 || ms < b.durMin {
		b.durMin = ms
	}
	if b.count == 1 || ms > b.durMax {
		b.durMax = ms
	}
	if len(b.samples) < maxSamples {
		b.samples = append(b.samples, ms)
		return
	}
	// Algorithm R: the n-th item replaces a random slot with probability
	// maxSamples/n, keeping every item equally likely to be retained.
	if j := rand.Int64N(b.count); j < maxSamples {
		b.samples[j] = ms
	}
}

// Recorder stores invocation metrics. The zero value is not usable; create
// one with New or NewWithClock. A Recorder is safe for concurrent use.
type Recorder struct {
	now func() time.Time

	mu        sync.Mutex
	series    map[seriesKey]map[int64]*bucket // keyed by minute start, unix seconds
	lastPrune time.Time
}

// New returns a Recorder that reads the wall clock.
func New() *Recorder {
	return NewWithClock(time.Now)
}

// NewWithClock returns a Recorder using now as its clock, which decides
// retention and the end of Snapshot windows. Tests inject a fake one.
func NewWithClock(now func() time.Time) *Recorder {
	if now == nil {
		now = time.Now
	}
	return &Recorder{now: now, series: make(map[seriesKey]map[int64]*bucket)}
}

// Record stores one invocation under the function-only series and under one
// series per distinct, non-empty entry of inv.Resources. A zero Start means
// "now". Invocations already older than Retention are discarded.
func (r *Recorder) Record(inv Invocation) {
	now := r.now()
	start := inv.Start
	if start.IsZero() {
		start = now
	}
	minute := start.UTC().Truncate(time.Minute)
	if minute.Before(now.Add(-Retention)) {
		return
	}
	key := minute.Unix()

	r.mu.Lock()
	defer r.mu.Unlock()
	r.prune(now)

	r.bucketFor(seriesKey{function: inv.Function}, key).add(inv)
	seen := make(map[string]bool, len(inv.Resources))
	for _, res := range inv.Resources {
		if res == "" || seen[res] {
			continue
		}
		seen[res] = true
		r.bucketFor(seriesKey{function: inv.Function, resource: res}, key).add(inv)
	}
}

// bucketFor returns the bucket of a series at a minute, creating it. The
// caller holds r.mu.
func (r *Recorder) bucketFor(k seriesKey, minute int64) *bucket {
	buckets := r.series[k]
	if buckets == nil {
		buckets = make(map[int64]*bucket)
		r.series[k] = buckets
	}
	b := buckets[minute]
	if b == nil {
		b = &bucket{}
		buckets[minute] = b
	}
	return b
}

// prune drops buckets older than Retention, and series left empty. It scans
// at most once a minute so recording stays cheap. The caller holds r.mu.
func (r *Recorder) prune(now time.Time) {
	if now.Sub(r.lastPrune) < time.Minute {
		return
	}
	r.lastPrune = now
	cutoff := now.Add(-Retention).UTC().Truncate(time.Minute).Unix()
	for k, buckets := range r.series {
		for minute := range buckets {
			if minute < cutoff {
				delete(buckets, minute)
			}
		}
		if len(buckets) == 0 {
			delete(r.series, k)
		}
	}
}

// durationMS converts a duration to fractional milliseconds.
func durationMS(d time.Duration) float64 {
	return float64(d) / float64(time.Millisecond)
}

// sortedKeys returns the bucket minutes of a series in ascending order.
func sortedKeys(buckets map[int64]*bucket) []int64 {
	keys := make([]int64, 0, len(buckets))
	for k := range buckets {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })
	return keys
}

package metrics

import (
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Statistic kinds ParseStat recognises, besides percentiles.
const (
	StatSum         = "Sum"
	StatSampleCount = "SampleCount"
	StatAverage     = "Average"
	StatMinimum     = "Minimum"
	StatMaximum     = "Maximum"
)

// Stat is a parsed CloudWatch statistic.
type Stat struct {
	kind       string  // one of the Stat constants, or "p" for a percentile
	percentile float64 // 0..100, only for kind "p"
}

// ParseStat parses "Sum", "SampleCount", "Average", "Minimum", "Maximum",
// "pNN" or "pNN.N" (a percentile between 0 and 100, for example "p99" or
// "p99.9"). Anything else is an error, so callers can reject a query before
// running it.
func ParseStat(s string) (Stat, error) {
	switch s {
	case StatSum, StatSampleCount, StatAverage, StatMinimum, StatMaximum:
		return Stat{kind: s}, nil
	}
	if rest, ok := strings.CutPrefix(s, "p"); ok && rest != "" {
		p, err := strconv.ParseFloat(rest, 64)
		if err == nil && p >= 0 && p <= 100 && !math.IsNaN(p) {
			return Stat{kind: "p", percentile: p}, nil
		}
	}
	return Stat{}, fmt.Errorf("unsupported statistic %q", s)
}

// Query selects one metric of one series and the statistic to compute.
type Query struct {
	// Metric is Invocations, Errors, Throttles or Duration.
	Metric string
	// Function is the FunctionName dimension value.
	Function string
	// Resource is the Resource dimension value, or "" for the series of the
	// function as a whole.
	Resource string
	// Start and End bound the query: Start is rounded down to the minute,
	// End is exclusive. Periods are aligned to the rounded Start.
	Start, End time.Time
	// Period is the datapoint width. Zero or negative means one minute.
	// Whole minute buckets are folded into the period that contains them.
	Period time.Duration
	// Stat is Sum, SampleCount, Average, Minimum, Maximum, pNN or pNN.N.
	Stat string
}

// Datapoint is one aggregated period of a query.
type Datapoint struct {
	// Timestamp is the start of the period (UTC).
	Timestamp time.Time
	Value     float64
}

// agg accumulates the buckets that fall into one period.
type agg struct {
	count, errors, throttles int64
	durSum, durMin, durMax   float64
	samples                  []float64
}

func (a *agg) merge(b *bucket) {
	if b.count > 0 {
		if a.count == 0 || b.durMin < a.durMin {
			a.durMin = b.durMin
		}
		if a.count == 0 || b.durMax > a.durMax {
			a.durMax = b.durMax
		}
	}
	a.count += b.count
	a.errors += b.errors
	a.throttles += b.throttles
	a.durSum += b.durSum
	a.samples = append(a.samples, b.samples...)
}

// Query aggregates the series selected by q into datapoints in ascending
// time order. It returns nil for an unknown metric or statistic, or when
// nothing matches.
//
// Periods with nothing to report are omitted, as CloudWatch omits them:
// Invocations, Errors and Duration need at least one invocation that ran in
// the period (so Errors reports 0, not a gap, for a healthy period), and
// Throttles needs an invocation or a throttle.
//
// For Duration the statistics are over the invocation durations in
// milliseconds: Sum is their total, SampleCount their number, Average,
// Minimum and Maximum as named, and percentiles use the nearest-rank method
// on the retained samples. For the count metrics every event contributes a
// value: Invocations records 1 per invocation, Errors 1 per failed and 0 per
// successful invocation, and Throttles 1 per throttle and 0 per invocation
// that ran. So Sum is the count itself (the figure CloudWatch alarms and
// dashboards use), SampleCount the number of events, Average the fraction
// of them (the error rate for Errors), and Minimum, Maximum and percentiles
// follow from those 0/1 values.
func (r *Recorder) Query(q Query) []Datapoint {
	stat, err := ParseStat(q.Stat)
	if err != nil {
		return nil
	}
	switch q.Metric {
	case Invocations, Errors, Throttles, Duration:
	default:
		return nil
	}
	period := q.Period
	if period <= 0 {
		period = defaultPeriod
	}
	origin := q.Start.UTC().Truncate(time.Minute)
	end := q.End.UTC()

	periods := map[int64]*agg{}
	r.mu.Lock()
	buckets := r.series[seriesKey{function: q.Function, resource: q.Resource}]
	for _, minute := range sortedKeys(buckets) {
		at := time.Unix(minute, 0).UTC()
		if at.Before(origin) || !at.Before(end) {
			continue
		}
		idx := int64(at.Sub(origin) / period)
		a := periods[idx]
		if a == nil {
			a = &agg{}
			periods[idx] = a
		}
		a.merge(buckets[minute])
	}
	r.mu.Unlock()

	idxs := make([]int64, 0, len(periods))
	for idx := range periods {
		idxs = append(idxs, idx)
	}
	sort.Slice(idxs, func(i, j int) bool { return idxs[i] < idxs[j] })

	var out []Datapoint
	for _, idx := range idxs {
		a := periods[idx]
		v, ok := a.value(q.Metric, stat)
		if !ok {
			continue
		}
		out = append(out, Datapoint{
			Timestamp: origin.Add(time.Duration(idx) * period),
			Value:     v,
		})
	}
	return out
}

// value computes the statistic for a metric, reporting false when the
// period has nothing to report for it.
func (a *agg) value(metric string, st Stat) (float64, bool) {
	switch metric {
	case Duration:
		if a.count == 0 {
			return 0, false
		}
		switch st.kind {
		case StatSum:
			return a.durSum, true
		case StatSampleCount:
			return float64(a.count), true
		case StatAverage:
			return a.durSum / float64(a.count), true
		case StatMinimum:
			return a.durMin, true
		case StatMaximum:
			return a.durMax, true
		}
		return nearestRank(a.samples, st.percentile), true
	case Invocations:
		if a.count == 0 {
			return 0, false
		}
		return binaryStat(a.count, 0, st), true
	case Errors:
		if a.count == 0 {
			return 0, false
		}
		return binaryStat(a.errors, a.count-a.errors, st), true
	default: // Throttles
		if a.count == 0 && a.throttles == 0 {
			return 0, false
		}
		return binaryStat(a.throttles, a.count, st), true
	}
}

// binaryStat computes a statistic over ones values of 1 and zeros values of
// 0, which is how the count metrics are modelled.
func binaryStat(ones, zeros int64, st Stat) float64 {
	total := ones + zeros
	switch st.kind {
	case StatSum:
		return float64(ones)
	case StatSampleCount:
		return float64(total)
	case StatAverage:
		return float64(ones) / float64(total)
	case StatMinimum:
		if zeros > 0 {
			return 0
		}
		return 1
	case StatMaximum:
		if ones > 0 {
			return 1
		}
		return 0
	}
	// Nearest rank over the sorted zeros-then-ones sequence.
	rank := int64(math.Ceil(st.percentile / 100 * float64(total)))
	if rank < 1 {
		rank = 1
	}
	if rank <= zeros {
		return 0
	}
	return 1
}

// nearestRank returns the nearest-rank percentile (0..100) of samples: the
// value at ordinal rank ceil(p/100*n) of the sorted list, with p=0 giving
// the minimum. It does not modify samples. Empty input yields 0.
func nearestRank(samples []float64, p float64) float64 {
	if len(samples) == 0 {
		return 0
	}
	sorted := append([]float64(nil), samples...)
	sort.Float64s(sorted)
	rank := int(math.Ceil(p / 100 * float64(len(sorted))))
	if rank < 1 {
		rank = 1
	}
	if rank > len(sorted) {
		rank = len(sorted)
	}
	return sorted[rank-1]
}

// DurationStats summarises durations in milliseconds.
type DurationStats struct {
	P50 float64 `json:"p50"`
	P95 float64 `json:"p95"`
	Max float64 `json:"max"`
}

// ResourceSnapshot holds the totals of one series over a Snapshot window.
type ResourceSnapshot struct {
	// Resource is the Resource dimension value; "" for a function's total.
	Resource    string        `json:"resource"`
	Invocations int64         `json:"invocations"`
	Errors      int64         `json:"errors"`
	Throttles   int64         `json:"throttles"`
	DurationMS  DurationStats `json:"duration_ms"`
}

// FunctionSnapshot holds one function's totals and its per-resource split.
type FunctionSnapshot struct {
	Name string `json:"name"`
	// Total covers the function-only series (Resource is "").
	Total ResourceSnapshot `json:"total"`
	// Resources has one entry per Resource value, sorted by name.
	Resources []ResourceSnapshot `json:"resources"`
}

// Snapshot totals every series over the trailing window ending now: whole
// minute buckets whose minute lies within [now-window, now] count. Functions
// and their resources are sorted by name. A non-positive window yields no
// functions.
func (r *Recorder) Snapshot(window time.Duration) []FunctionSnapshot {
	if window <= 0 {
		return nil
	}
	now := r.now()
	from := now.Add(-window).UTC().Truncate(time.Minute)

	r.mu.Lock()
	defer r.mu.Unlock()

	byFn := map[string]*FunctionSnapshot{}
	for k, buckets := range r.series {
		var a agg
		for minute, b := range buckets {
			at := time.Unix(minute, 0).UTC()
			if at.Before(from) || at.After(now) {
				continue
			}
			a.merge(b)
		}
		if a.count == 0 && a.throttles == 0 {
			continue
		}
		rs := ResourceSnapshot{
			Resource:    k.resource,
			Invocations: a.count,
			Errors:      a.errors,
			Throttles:   a.throttles,
		}
		if a.count > 0 {
			rs.DurationMS = DurationStats{
				P50: nearestRank(a.samples, 50),
				P95: nearestRank(a.samples, 95),
				Max: a.durMax,
			}
		}
		fs := byFn[k.function]
		if fs == nil {
			fs = &FunctionSnapshot{Name: k.function, Resources: []ResourceSnapshot{}}
			byFn[k.function] = fs
		}
		if k.resource == "" {
			fs.Total = rs
		} else {
			fs.Resources = append(fs.Resources, rs)
		}
	}

	out := make([]FunctionSnapshot, 0, len(byFn))
	for _, fs := range byFn {
		sort.Slice(fs.Resources, func(i, j int) bool { return fs.Resources[i].Resource < fs.Resources[j].Resource })
		out = append(out, *fs)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

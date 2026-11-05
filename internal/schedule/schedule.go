// Package schedule parses AWS EventBridge schedule expressions — rate(),
// cron() and at() — and computes when each fires next, so `lambdary dev` can
// run scheduled functions on the same expressions they are deployed with.
package schedule

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"
)

// ErrInvalid wraps every parse error, so callers can match on it.
var ErrInvalid = errors.New("invalid schedule expression")

// kind tells the three expression forms apart.
type kind int

const (
	kindRate kind = iota
	kindCron
	kindAt
)

// Expr is a parsed schedule expression. The zero value is not usable; build
// one with Parse.
type Expr struct {
	src  string
	kind kind

	every time.Duration // rate
	cron  *cronSpec     // cron
	at    time.Time     // at, as wall-clock fields; the location is applied by Next
}

// String returns the expression as written.
func (e *Expr) String() string { return e.src }

// Parse parses an EventBridge schedule expression:
//
//   - rate(value unit), unit one of minute(s), hour(s), day(s), singular for 1
//     and plural otherwise, as AWS requires.
//   - cron(minutes hours day-of-month month day-of-week year), in AWS's
//     six-field syntax: day-of-week counts 1 (SUN) to 7 (SAT), one of the two
//     day fields must be "?", and names such as JAN or MON-FRI are accepted.
//     The L, W and # day modifiers are not supported locally.
//   - at(yyyy-mm-ddThh:mm:ss), a one-time schedule.
func Parse(expr string) (*Expr, error) {
	s := strings.TrimSpace(expr)

	open := strings.IndexByte(s, '(')
	if open < 0 || !strings.HasSuffix(s, ")") {
		return nil, fmt.Errorf("%w %q: want rate(...), cron(...) or at(...)", ErrInvalid, expr)
	}

	body := strings.TrimSpace(s[open+1 : len(s)-1])

	var (
		e   = &Expr{src: s}
		err error
	)

	switch s[:open] {
	case "rate":
		e.kind = kindRate
		e.every, err = parseRate(body)
	case "cron":
		e.kind = kindCron
		e.cron, err = parseCron(body)
	case "at":
		e.kind = kindAt
		e.at, err = time.Parse("2006-01-02T15:04:05", body)
		if err != nil {
			err = errors.New("want at(yyyy-mm-ddThh:mm:ss)")
		}
	default:
		err = errors.New("want rate(...), cron(...) or at(...)")
	}

	if err != nil {
		return nil, fmt.Errorf("%w %q: %s", ErrInvalid, expr, err)
	}

	return e, nil
}

// Next returns the first time after t that e fires, evaluated in loc (UTC
// when nil), and false when it never fires again. A rate fires every period
// counted from t, so passing the previous fire time keeps a steady cadence.
func (e *Expr) Next(t time.Time, loc *time.Location) (time.Time, bool) {
	if loc == nil {
		loc = time.UTC
	}

	switch e.kind {
	case kindRate:
		return t.Add(e.every), true
	case kindAt:
		at := time.Date(e.at.Year(), e.at.Month(), e.at.Day(), e.at.Hour(), e.at.Minute(), e.at.Second(), 0, loc)
		return at, at.After(t)
	default:
		return e.cron.next(t.In(loc))
	}
}

// parseRate parses rate()'s "value unit" body.
func parseRate(body string) (time.Duration, error) {
	fields := strings.Fields(body)
	if len(fields) != 2 {
		return 0, errors.New("want rate(value unit)")
	}

	n, err := strconv.Atoi(fields[0])
	if err != nil || n <= 0 {
		return 0, errors.New("the value must be a positive whole number")
	}

	unit := fields[1]

	var base time.Duration

	switch strings.TrimSuffix(unit, "s") {
	case "minute":
		base = time.Minute
	case "hour":
		base = time.Hour
	case "day":
		base = 24 * time.Hour
	default:
		return 0, fmt.Errorf("unit %q must be minute(s), hour(s) or day(s)", unit)
	}

	if plural := strings.HasSuffix(unit, "s"); plural != (n > 1) {
		return 0, fmt.Errorf("AWS wants a singular unit for 1 and a plural one otherwise, got %q", body)
	}

	return time.Duration(n) * base, nil
}

// cronSpec holds the allowed values of each cron field. A nil day set means
// "?": that field doesn't constrain the day.
type cronSpec struct {
	minutes, hours, months, years []bool
	dom, dow                      []bool
}

// field describes one cron field's range and names.
type field struct {
	name     string
	min, max int
	names    []string // names[i] is value min+i
}

var (
	monthNames = []string{"JAN", "FEB", "MAR", "APR", "MAY", "JUN", "JUL", "AUG", "SEP", "OCT", "NOV", "DEC"}
	dayNames   = []string{"SUN", "MON", "TUE", "WED", "THU", "FRI", "SAT"}

	minuteField = field{name: "minutes", min: 0, max: 59}
	hourField   = field{name: "hours", min: 0, max: 23}
	domField    = field{name: "day-of-month", min: 1, max: 31}
	monthField  = field{name: "month", min: 1, max: 12, names: monthNames}
	dowField    = field{name: "day-of-week", min: 1, max: 7, names: dayNames}
	yearField   = field{name: "year", min: 1970, max: 2199}
)

// parseCron parses cron()'s six-field body.
func parseCron(body string) (*cronSpec, error) {
	parts := strings.Fields(body)
	if len(parts) != 6 {
		return nil, fmt.Errorf("want 6 fields (minutes hours day-of-month month day-of-week year), got %d", len(parts))
	}

	domAny, dowAny := parts[2] == "?", parts[4] == "?"
	if domAny == dowAny {
		return nil, errors.New(`exactly one of day-of-month and day-of-week must be "?"`)
	}

	spec := &cronSpec{}

	var err error

	for _, f := range []struct {
		dst   *[]bool
		src   string
		field field
	}{
		{&spec.minutes, parts[0], minuteField},
		{&spec.hours, parts[1], hourField},
		{&spec.months, parts[3], monthField},
		{&spec.years, parts[5], yearField},
	} {
		if *f.dst, err = parseField(f.src, f.field); err != nil {
			return nil, err
		}
	}

	if !domAny {
		if spec.dom, err = parseField(parts[2], domField); err != nil {
			return nil, err
		}
	}

	if !dowAny {
		if spec.dow, err = parseField(parts[4], dowField); err != nil {
			return nil, err
		}
	}

	return spec, nil
}

// parseField parses one comma-separated cron field into a set indexed by
// value - f.min.
func parseField(src string, f field) ([]bool, error) {
	if !onlyNames(src, f) {
		return nil, fmt.Errorf("%s %q: the L, W and # modifiers are not supported locally", f.name, src)
	}

	set := make([]bool, f.max-f.min+1)

	for _, item := range strings.Split(src, ",") {
		rng, stepStr, hasStep := strings.Cut(item, "/")

		step := 1
		if hasStep {
			n, err := strconv.Atoi(stepStr)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("%s %q: bad step %q", f.name, src, stepStr)
			}
			step = n
		}

		lo, hi := f.min, f.max

		switch {
		case rng == "*":
		case strings.Contains(rng, "-"):
			a, b, _ := strings.Cut(rng, "-")

			var err error
			if lo, err = value(a, f); err != nil {
				return nil, fmt.Errorf("%s %q: %w", f.name, src, err)
			}
			if hi, err = value(b, f); err != nil {
				return nil, fmt.Errorf("%s %q: %w", f.name, src, err)
			}
			if hi < lo {
				return nil, fmt.Errorf("%s %q: range %s runs backwards", f.name, src, rng)
			}
		default:
			v, err := value(rng, f)
			if err != nil {
				return nil, fmt.Errorf("%s %q: %w", f.name, src, err)
			}

			// "5/15" starts at 5 and runs to the end; a plain "5" is just 5.
			lo = v
			if !hasStep {
				hi = v
			}
		}

		for v := lo; v <= hi; v += step {
			set[v-f.min] = true
		}
	}

	return set, nil
}

// onlyNames reports whether src is free of the L, W and # modifiers once f's
// names, whose letters (JUL, WED, ...) look like them, are taken out.
func onlyNames(src string, f field) bool {
	stripped := strings.ToUpper(src)
	for _, n := range f.names {
		stripped = strings.ReplaceAll(stripped, n, "")
	}

	return !strings.ContainsAny(stripped, "LW#")
}

// value parses one cron value, a number or one of f's names.
func value(s string, f field) (int, error) {
	for i, n := range f.names {
		if strings.EqualFold(s, n) {
			return f.min + i, nil
		}
	}

	v, err := strconv.Atoi(s)
	if err != nil {
		return 0, fmt.Errorf("bad value %q", s)
	}

	if v < f.min || v > f.max {
		return 0, fmt.Errorf("value %d is outside %d-%d", v, f.min, f.max)
	}

	return v, nil
}

// next returns the first minute strictly after t matching s, in t's location.
func (s *cronSpec) next(t time.Time) (time.Time, bool) {
	loc := t.Location()
	t = t.Truncate(time.Minute).Add(time.Minute)

	for t.Year() <= yearField.max {
		switch {
		case !s.years[t.Year()-yearField.min]:
			t = time.Date(t.Year()+1, 1, 1, 0, 0, 0, 0, loc)
		case !s.months[int(t.Month())-monthField.min]:
			t = time.Date(t.Year(), t.Month()+1, 1, 0, 0, 0, 0, loc)
		case !s.dayMatches(t):
			t = time.Date(t.Year(), t.Month(), t.Day()+1, 0, 0, 0, 0, loc)
		case !s.hours[t.Hour()]:
			t = time.Date(t.Year(), t.Month(), t.Day(), t.Hour()+1, 0, 0, 0, loc)
		case !s.minutes[t.Minute()]:
			t = t.Add(time.Minute)
		default:
			return t, true
		}
	}

	return time.Time{}, false
}

// dayMatches checks whichever day field isn't "?".
func (s *cronSpec) dayMatches(t time.Time) bool {
	if s.dom != nil {
		return s.dom[t.Day()-domField.min]
	}

	// time.Weekday counts Sunday as 0; AWS counts it as 1.
	return s.dow[int(t.Weekday())]
}

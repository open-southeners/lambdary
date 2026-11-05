package schedule

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func mustTime(t *testing.T, s string) time.Time {
	t.Helper()

	v, err := time.Parse(time.RFC3339, s)
	if err != nil {
		t.Fatalf("parsing %s: %v", s, err)
	}

	return v
}

func TestParseErrors(t *testing.T) {
	tests := []struct {
		expr string
		want string
	}{
		{"every minute", "want rate"},
		{"rate(1 minutes)", "singular"},
		{"rate(5 minute)", "plural"},
		{"rate(0 minutes)", "positive"},
		{"rate(1 second)", "unit"},
		{"rate(1)", "value unit"},
		{"cron(0 12 * * ? )", "6 fields"},
		{"cron(0 12 * * * *)", `"?"`},
		{"cron(0 12 ? * ? *)", `"?"`},
		{"cron(60 12 * * ? *)", "outside"},
		{"cron(0 12 L * ? *)", "not supported"},
		{"cron(0 12 ? * 6#3 *)", "not supported"},
		{"cron(0 12 ? * 2-1 *)", "backwards"},
		{"cron(*/0 12 * * ? *)", "step"},
		{"at(2026-11-05 10:00)", "at(yyyy"},
		{"hourly(1)", "want rate"},
	}

	for _, tt := range tests {
		t.Run(tt.expr, func(t *testing.T) {
			_, err := Parse(tt.expr)
			if err == nil {
				t.Fatalf("Parse(%q) succeeded, want an error mentioning %q", tt.expr, tt.want)
			}

			if !errors.Is(err, ErrInvalid) {
				t.Errorf("Parse(%q) error %v does not wrap ErrInvalid", tt.expr, err)
			}

			if !strings.Contains(err.Error(), tt.want) {
				t.Errorf("Parse(%q) error = %q, want it to mention %q", tt.expr, err, tt.want)
			}
		})
	}
}

func TestNext(t *testing.T) {
	tests := []struct {
		expr  string
		after string
		want  string // "" means never
	}{
		{"rate(1 minute)", "2026-11-05T10:00:30Z", "2026-11-05T10:01:30Z"},
		{"rate(5 minutes)", "2026-11-05T10:00:00Z", "2026-11-05T10:05:00Z"},
		{"rate(2 hours)", "2026-11-05T10:00:00Z", "2026-11-05T12:00:00Z"},
		{"rate(1 day)", "2026-11-05T10:00:00Z", "2026-11-06T10:00:00Z"},
		{"cron(* * * * ? *)", "2026-11-05T10:00:30Z", "2026-11-05T10:01:00Z"},
		{"cron(0/15 * * * ? *)", "2026-11-05T10:01:00Z", "2026-11-05T10:15:00Z"},
		{"cron(*/15 * * * ? *)", "2026-11-05T10:45:00Z", "2026-11-05T11:00:00Z"},
		{"cron(0 12 * * ? *)", "2026-11-05T12:00:00Z", "2026-11-06T12:00:00Z"},
		{"cron(15 10 ? * MON-FRI *)", "2026-11-06T11:00:00Z", "2026-11-09T10:15:00Z"}, // Fri → Mon
		{"cron(0 8 ? * 1 *)", "2026-11-05T00:00:00Z", "2026-11-08T08:00:00Z"},         // 1 is Sunday
		{"cron(0 0 1 JAN,JUL ? *)", "2026-11-05T00:00:00Z", "2027-01-01T00:00:00Z"},
		{"cron(0 0 31 * ? *)", "2026-11-05T00:00:00Z", "2026-12-31T00:00:00Z"}, // November has no 31st
		{"cron(0 0 1 1 ? 2025)", "2026-11-05T00:00:00Z", ""},
		{"at(2026-11-05T12:00:00)", "2026-11-05T10:00:00Z", "2026-11-05T12:00:00Z"},
		{"at(2026-11-05T12:00:00)", "2026-11-05T12:00:00Z", ""},
	}

	for _, tt := range tests {
		t.Run(tt.expr+"@"+tt.after, func(t *testing.T) {
			e, err := Parse(tt.expr)
			if err != nil {
				t.Fatalf("Parse: %v", err)
			}

			got, ok := e.Next(mustTime(t, tt.after), nil)

			if tt.want == "" {
				if ok {
					t.Fatalf("Next = %s, want never", got)
				}
				return
			}

			if !ok {
				t.Fatalf("Next = never, want %s", tt.want)
			}

			if want := mustTime(t, tt.want); !got.Equal(want) {
				t.Errorf("Next = %s, want %s", got.UTC().Format(time.RFC3339), tt.want)
			}
		})
	}
}

func TestNextInLocation(t *testing.T) {
	loc, err := time.LoadLocation("Europe/Madrid")
	if err != nil {
		t.Skipf("no tzdata: %v", err)
	}

	e, err := Parse("cron(0 9 * * ? *)")
	if err != nil {
		t.Fatal(err)
	}

	// 09:00 in Madrid in November is 08:00 UTC.
	got, _ := e.Next(mustTime(t, "2026-11-05T00:00:00Z"), loc)
	if want := mustTime(t, "2026-11-05T08:00:00Z"); !got.Equal(want) {
		t.Errorf("Next = %s, want %s", got.UTC(), want)
	}
}

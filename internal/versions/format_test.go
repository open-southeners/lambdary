package versions

import (
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

// stateFixture returns a state.json with one published version whose manifest
// is manifestJSON, preceded by format (empty for none, as format 1 wrote it).
func stateFixture(format, manifestJSON string) string {
	return fmt.Sprintf(`{
  %s"next_version": 2,
  "revision_id": "r",
  "versions": [{"version": "1", "runtime": "nodejs22.x", "handler": "index.handler", "manifest": %s}],
  "aliases": {}
}`, format, manifestJSON)
}

// TestLegacyTimeoutReadsAsUnset checks that a version snapshotted by code
// that stored Timeout as a plain int reads a 0 or missing timeout as unset
// (900 s), while a format 2 snapshot keeps an explicit 0 as no limit.
func TestLegacyTimeoutReadsAsUnset(t *testing.T) {
	tests := []struct {
		name     string
		format   string
		manifest string
		want     int
	}{
		{"legacy 0 is unset", "", `{"Timeout": 0, "Memory": 256}`, 900},
		{"legacy field absent is unset", "", `{"Memory": 256}`, 900},
		{"legacy null manifest is unset", "", `null`, 900},
		{"legacy 15 is kept", "", `{"Timeout": 15}`, 15},
		{"format 2 explicit 0 is no limit", `"format": 2,`, `{"Timeout": 0}`, 0},
		{"format 2 1800 is kept", `"format": 2,`, `{"Timeout": 1800}`, 1800},
		{"format 2 null is unset", `"format": 2,`, `{"Timeout": null}`, 900},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			s := NewStore(t.TempDir())
			writeFile(t, filepath.Join(s.functionDir("hello"), "state.json"), stateFixture(tc.format, tc.manifest), 0o644)

			list, err := s.List("hello")
			if err != nil {
				t.Fatal(err)
			}

			if got := list[0].Manifest.TimeoutSeconds(); got != tc.want {
				t.Errorf("List timeout = %d, want %d", got, tc.want)
			}

			fn, err := s.Function("hello", "1")
			if err != nil {
				t.Fatal(err)
			}

			if got := fn.Manifest.TimeoutSeconds(); got != tc.want {
				t.Errorf("Function timeout = %d, want %d", got, tc.want)
			}
		})
	}
}

// TestSaveWritesFormat checks that a legacy state saved again carries the
// new format and keeps its version's timeout as unset, and that a version
// published with an explicit 0 reads back as no limit.
func TestSaveWritesFormat(t *testing.T) {
	s := NewStore(t.TempDir())
	writeFile(t, filepath.Join(s.functionDir("hello"), "state.json"), stateFixture("", `{"Timeout": 0}`), 0o644)

	if _, err := s.CreateAlias("hello", "live", "1", "", nil); err != nil {
		t.Fatal(err)
	}

	if got := readFile(t, filepath.Join(s.functionDir("hello"), "state.json")); !strings.Contains(got, `"format": 2`) {
		t.Errorf("state.json after save lacks format 2:\n%s", got)
	}

	list, err := s.List("hello")
	if err != nil {
		t.Fatal(err)
	}

	if got := list[0].Manifest.TimeoutSeconds(); got != 900 {
		t.Errorf("legacy timeout after resave = %d, want 900", got)
	}

	fn := newFn(t, "zero")
	fn.Manifest.Timeout = new(0)

	publish(t, s, fn)

	reread, err := s.List("zero")
	if err != nil {
		t.Fatal(err)
	}

	if got := reread[0].Manifest.TimeoutSeconds(); got != 0 {
		t.Errorf("explicit 0 after reload = %d, want 0 (no limit)", got)
	}
}

package backend

import (
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func TestParseEnvFile(t *testing.T) {
	cases := []struct {
		name    string
		content string
		want    map[string]string
	}{
		{
			name:    "simple KEY=VALUE pairs",
			content: "FOO=bar\nBAZ=qux\n",
			want:    map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name:    "blank lines and full-line comments are skipped",
			content: "\n# a comment\nFOO=bar\n\n# another\nBAZ=qux\n",
			want:    map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name:    "optional export prefix is stripped",
			content: "export FOO=bar\nBAZ=qux\n",
			want:    map[string]string{"FOO": "bar", "BAZ": "qux"},
		},
		{
			name:    "double-quoted value has its quotes stripped",
			content: `FOO="bar baz"` + "\n",
			want:    map[string]string{"FOO": "bar baz"},
		},
		{
			name:    "single-quoted value has its quotes stripped",
			content: "FOO='bar baz'\n",
			want:    map[string]string{"FOO": "bar baz"},
		},
		{
			name:    "no escape sequences are interpreted inside quotes",
			content: `FOO="a\nb"` + "\n",
			want:    map[string]string{"FOO": `a\nb`},
		},
		{
			name:    "trailing text after an unquoted value stays part of the value (no trailing comments)",
			content: "FOO=bar # not a comment\n",
			want:    map[string]string{"FOO": "bar # not a comment"},
		},
		{
			name:    "surrounding whitespace around the line and value is trimmed",
			content: "  FOO = bar  \n",
			want:    map[string]string{"FOO": "bar"},
		},
		{
			name:    "empty value is allowed",
			content: "FOO=\n",
			want:    map[string]string{"FOO": ""},
		},
		{
			name:    "later duplicate key wins",
			content: "FOO=first\nFOO=second\n",
			want:    map[string]string{"FOO": "second"},
		},
		{
			name:    "empty file yields an empty, non-nil map",
			content: "",
			want:    map[string]string{},
		},
		{
			name:    "value itself containing an = is kept whole (only the first = splits)",
			content: "FOO=bar=baz\n",
			want:    map[string]string{"FOO": "bar=baz"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempEnvFile(t, tc.content)

			got, err := ParseEnvFile(path)
			if err != nil {
				t.Fatalf("ParseEnvFile() unexpected error: %v", err)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Errorf("ParseEnvFile() =\n%v\nwant\n%v", got, tc.want)
			}
		})
	}
}

func TestParseEnvFileMalformedLines(t *testing.T) {
	cases := []struct {
		name     string
		content  string
		wantLine int
	}{
		{
			name:     "line with no = errors naming the line number",
			content:  "FOO=bar\nNOTANASSIGNMENT\n",
			wantLine: 2,
		},
		{
			name:     "line with an empty key errors naming the line number",
			content:  "FOO=bar\n=novalue\n",
			wantLine: 2,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTempEnvFile(t, tc.content)

			_, err := ParseEnvFile(path)
			if err == nil {
				t.Fatal("ParseEnvFile() expected error, got nil")
			}
			if !strings.Contains(err.Error(), "line "+strconv.Itoa(tc.wantLine)) {
				t.Errorf("ParseEnvFile() error = %v, want it to name line %d", err, tc.wantLine)
			}
		})
	}
}

func TestParseEnvFileMissing(t *testing.T) {
	_, err := ParseEnvFile(filepath.Join(t.TempDir(), "does-not-exist.env"))
	if err == nil {
		t.Fatal("ParseEnvFile() expected error for a missing file, got nil")
	}
}

// writeTempEnvFile writes content to a fresh file under t.TempDir() and
// returns its path, failing the test on any error.
func writeTempEnvFile(t *testing.T, content string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "test.env")
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}

	return path
}

package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
)

func TestRenderListTable(t *testing.T) {
	fns := []discovery.Function{
		{
			Name:    "api",
			Dir:     "functions/api",
			Runtime: "nodejs22.x",
			Route:   "/api",
			Backend: "auto",
		},
		{
			Name:    "worker",
			Dir:     "functions/worker",
			Runtime: "python3.13",
			Route:   "/worker",
			Backend: "container",
		},
	}

	var out, errOut bytes.Buffer
	renderList(&out, &errOut, fns, "functions")

	if errOut.Len() != 0 {
		t.Errorf("errOut = %q, want empty", errOut.String())
	}

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 3 {
		t.Fatalf("got %d lines, want 3 (header + 2 rows):\n%s", len(lines), out.String())
	}

	header := lines[0]
	for _, col := range []string{"NAME", "RUNTIME", "BACKEND", "ROUTE", "DIR"} {
		if !strings.Contains(header, col) {
			t.Errorf("header %q missing column %q", header, col)
		}
	}

	// Columns must line up: every row's fields start at the same offset
	// as the header's, which tabwriter guarantees by padding with
	// spaces. Check by splitting on runs of whitespace and comparing
	// field counts/content instead of exact byte offsets, since column
	// widths depend on the longest value in each column.
	wantRows := [][]string{
		{"api", "nodejs22.x", "auto", "/api", "api"},
		{"worker", "python3.13", "container", "/worker", "worker"},
	}
	for i, want := range wantRows {
		got := strings.Fields(lines[i+1])
		if len(got) != len(want) {
			t.Fatalf("row %d = %q, want fields %v", i, lines[i+1], want)
		}
		for j, w := range want {
			if got[j] != w {
				t.Errorf("row %d field %d = %q, want %q", i, j, got[j], w)
			}
		}
	}

	// Header/row columns should share alignment: the start offset of
	// each column in the header matches the corresponding row.
	nameIdx := strings.Index(header, "NAME")
	runtimeIdx := strings.Index(header, "RUNTIME")
	if nameIdx != strings.Index(lines[1], "api") {
		t.Errorf("NAME column misaligned: header at %d, row at %d", nameIdx, strings.Index(lines[1], "api"))
	}
	if runtimeIdx != strings.Index(lines[1], "nodejs22.x") {
		t.Errorf("RUNTIME column misaligned: header at %d, row at %d", runtimeIdx, strings.Index(lines[1], "nodejs22.x"))
	}
}

func TestRenderListEmptyRuntime(t *testing.T) {
	fns := []discovery.Function{
		{Name: "container-fn", Dir: "functions/container-fn", Runtime: "", Route: "/container-fn", Backend: "container"},
	}

	var out, errOut bytes.Buffer
	renderList(&out, &errOut, fns, "functions")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	if len(lines) != 2 {
		t.Fatalf("got %d lines, want 2 (header + 1 row):\n%s", len(lines), out.String())
	}

	fields := strings.Fields(lines[1])
	if len(fields) < 2 || fields[1] != "-" {
		t.Errorf("row = %q, want runtime column to be \"-\"", lines[1])
	}
}

func TestRenderListWarnings(t *testing.T) {
	fns := []discovery.Function{
		{
			Name:     "api",
			Dir:      "functions/api",
			Runtime:  "nodejs22.x",
			Route:    "/api",
			Backend:  "auto",
			Warnings: []string{`route "/api" is shared by functions: api, api2`},
		},
		{
			Name:     "api2",
			Dir:      "functions/api2",
			Runtime:  "nodejs22.x",
			Route:    "/api",
			Backend:  "auto",
			Warnings: []string{`route "/api" is shared by functions: api, api2`},
		},
	}

	var out, errOut bytes.Buffer
	renderList(&out, &errOut, fns, "functions")

	wantWarning := "warning: " + `route "/api" is shared by functions: api, api2`
	gotLines := strings.Split(strings.TrimRight(errOut.String(), "\n"), "\n")
	if len(gotLines) != 2 {
		t.Fatalf("errOut lines = %v, want 2 (one per function)", gotLines)
	}
	for _, line := range gotLines {
		if line != wantWarning {
			t.Errorf("errOut line = %q, want %q", line, wantWarning)
		}
	}

	// The table itself is unaffected by warnings.
	if !strings.Contains(out.String(), "NAME") {
		t.Errorf("out = %q, want a table header", out.String())
	}
}

func TestRenderListEmpty(t *testing.T) {
	var out, errOut bytes.Buffer
	renderList(&out, &errOut, nil, "testdata/empty")

	if out.Len() != 0 {
		t.Errorf("out = %q, want empty (no table)", out.String())
	}

	want := "no functions found under testdata/empty\n"
	if errOut.String() != want {
		t.Errorf("errOut = %q, want %q", errOut.String(), want)
	}
}

func TestRenderListDirRelativeToScanRoot(t *testing.T) {
	fns := []discovery.Function{
		{Name: "api", Dir: "root/api", Runtime: "nodejs22.x", Route: "/api", Backend: "auto"},
	}

	var out, errOut bytes.Buffer
	renderList(&out, &errOut, fns, "root")

	lines := strings.Split(strings.TrimRight(out.String(), "\n"), "\n")
	fields := strings.Fields(lines[1])
	dir := fields[len(fields)-1]
	if dir != "api" {
		t.Errorf("dir = %q, want %q (relative to scan root)", dir, "api")
	}
}

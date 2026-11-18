package cli

import (
	"bytes"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestAwsFor(t *testing.T) {
	if got := awsFor(nil, 8000, "127.0.0.1"); got != (backend.AWS{}) {
		t.Errorf("no config: awsFor() = %+v, want zero", got)
	}

	if got := awsFor(&manifest.Config{}, 8000, "127.0.0.1"); got != (backend.AWS{}) {
		t.Errorf("option off: awsFor() = %+v, want zero", got)
	}

	on := &manifest.Config{AWS: manifest.AWS{Environment: true}}

	if got, want := awsFor(on, 8000, "127.0.0.1"), (backend.AWS{Region: "us-east-1", Port: 8000, LoopbackOnly: true}); got != want {
		t.Errorf("awsFor() = %+v, want %+v", got, want)
	}

	if got := awsFor(on, 8000, "0.0.0.0"); got.LoopbackOnly {
		t.Errorf("0.0.0.0 counted as loopback: %+v", got)
	}

	// Standalone invoke: no server, so no endpoint.
	if got := awsFor(on, 0, ""); got.Port != 0 || !got.Enabled() {
		t.Errorf("standalone awsFor() = %+v, want enabled with no port", got)
	}
}

func TestIsLoopbackHost(t *testing.T) {
	for host, want := range map[string]bool{
		"127.0.0.1": true, "127.0.0.2": true, "::1": true, "localhost": true,
		"0.0.0.0": false, "": false, "192.168.1.5": false, "::": false,
	} {
		if got := isLoopbackHost(host); got != want {
			t.Errorf("isLoopbackHost(%q) = %v, want %v", host, got, want)
		}
	}
}

func TestWarnAWSLoopback(t *testing.T) {
	loopback := backend.AWS{Region: "us-east-1", Port: 8000, LoopbackOnly: true}

	for _, tc := range []struct {
		name string
		aws  backend.AWS
		p    container.Placement
		warn bool
	}{
		{"host, loopback only", loopback, container.Placement{}, true},
		{"host, reachable listen address", backend.AWS{Region: "r", Port: 8000}, container.Placement{}, false},
		{"lambdary in a container", loopback, container.Placement{Direct: true}, false},
		{"option off", backend.AWS{}, container.Placement{}, false},
		{"standalone, no server", backend.AWS{Region: "r", LoopbackOnly: true}, container.Placement{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var buf bytes.Buffer

			warnAWSLoopback(&buf, tc.aws, tc.p)

			if got := buf.Len() > 0; got != tc.warn {
				t.Errorf("warned = %v, want %v (output %q)", got, tc.warn, buf.String())
			}

			if tc.warn && !strings.Contains(buf.String(), "--host 0.0.0.0") {
				t.Errorf("warning %q doesn't suggest --host 0.0.0.0", buf.String())
			}
		})
	}
}

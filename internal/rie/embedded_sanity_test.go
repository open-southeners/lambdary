//go:build embedrie

package rie

import "testing"

// TestEmbeddedPayloadPresent is CI's cross-check that an embedrie build
// actually embedded something: it only compiles under the embedrie tag
// (embedded.go's go:embed directive would already have failed the build if
// internal/rie/embedded/aws-lambda-rie were missing or empty, but this test
// catches the payload silently coming through empty some other way — e.g. a
// zero-byte file that go:embed happily accepts). The release workflow runs
// this alongside the native linux/amd64 build (see .github/workflows/
// release.yml's binaries job) as an extra guard beyond the existing
// `lambdary --version` sanity check.
func TestEmbeddedPayloadPresent(t *testing.T) {
	if len(embeddedRIE) == 0 {
		t.Fatal("embeddedRIE is empty under the embedrie build tag — internal/rie/embedded/aws-lambda-rie was missing, empty, or the embed didn't happen")
	}
}

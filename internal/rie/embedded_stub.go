//go:build !embedrie

package rie

// embeddedRIE is nil on every build that doesn't set the embedrie tag (plain
// `go build`, `go install`) — dev builds have no aws-lambda-rie binary on
// disk to embed, so Resolve's embedded-extract step is a no-op and the chain
// falls through to build-from-source, exactly as before this file existed.
var embeddedRIE []byte

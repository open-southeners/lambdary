//go:build embedrie

package rie

import _ "embed"

// embeddedRIE holds the aws-lambda-rie binary CI places at
// internal/rie/embedded/aws-lambda-rie before building lambdary with
// -tags embedrie (see .github/workflows/release.yml's binaries job). It's
// per-target: the release workflow rebuilds this file for each GOOS/GOARCH
// immediately before compiling lambdary for that target, so the payload
// embedded here always matches the binary it ends up inside.
//
// go:embed fails the build if the file is missing or empty — acceptable
// because this file only compiles under the embedrie tag, which only CI
// sets, and only after populating internal/rie/embedded/aws-lambda-rie.
//
//go:embed embedded/aws-lambda-rie
var embeddedRIE []byte

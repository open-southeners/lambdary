package backend

import (
	"fmt"
	"regexp"
)

// imageRegistry is the public ECR registry AWS publishes its official
// Lambda base images under, per DESIGN.md's "Container backend" section.
const imageRegistry = "public.ecr.aws/lambda"

// runtimeImages maps AWS Lambda runtime identifiers to an explicit image
// tag, for runtimes that either don't fit the family+version pattern
// runtimePattern handles (provided.* custom runtimes name an OS, not a
// numeric version) or that M1 specifically verifies against.
var runtimeImages = map[string]string{
	"nodejs22.x":      "nodejs:22",
	"python3.13":      "python:3.13",
	"ruby3.3":         "ruby:3.3",
	"dotnet8":         "dotnet:8",
	"provided.al2023": "provided:al2023",
	"provided.al2":    "provided:al2",
}

// runtimePattern matches the regular shape most AWS runtime ids follow: a
// lowercase family name, a numeric (optionally dotted) version, and an
// optional ".x" suffix — e.g. "nodejs20.x", "python3.12", "java21".
var runtimePattern = regexp.MustCompile(`^([a-z]+)(\d+(?:\.\d+)?)(?:\.x)?$`)

// ImageFor maps an AWS Lambda runtime identifier to the public AWS ECR
// image that runs it locally, per DESIGN.md's "Container backend" naming:
// `public.ecr.aws/lambda/{family}:{version}`. runtimeImages is checked
// first; anything not listed there is parsed against runtimePattern, so
// regular AWS runtime ids (including ones not yet added to the table, e.g.
// a future "nodejs24.x") resolve without a code change. Runtimes that
// match neither — including malformed or unrecognized ids — return an
// error naming the runtime.
func ImageFor(runtime string) (string, error) {
	if tag, ok := runtimeImages[runtime]; ok {
		return imageRegistry + "/" + tag, nil
	}

	m := runtimePattern.FindStringSubmatch(runtime)
	if m == nil {
		return "", fmt.Errorf("backend: no known container image for runtime %q", runtime)
	}

	family, version := m[1], m[2]

	return fmt.Sprintf("%s/%s:%s", imageRegistry, family, version), nil
}

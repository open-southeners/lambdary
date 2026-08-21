// Package manifest implements parsing and validation for `.lambda.yml`
// function manifests and the root-level `lambdary.yml` project config
// described in DESIGN.md's ".lambda.yml spec (v0)" section.
package manifest

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	"gopkg.in/yaml.v3"
)

// DefaultPayload is the Lambda Function URL / API Gateway event payload
// format assumed when a manifest sets url.path but not url.payload. It is
// filled in by (*Manifest).ApplyBuiltinDefaults, not Load — see that
// method's doc comment for the intended layering order.
const DefaultPayload = "2.0"

// Errors returned by Manifest and Defaults validation. They are wrapped
// with the offending file path and value by Load, so callers can match on
// them with errors.Is.
var (
	// ErrInvalidBackend indicates local.backend is set to a value other
	// than "", "auto", "container", or "process".
	ErrInvalidBackend = errors.New(`local.backend must be one of "", "auto", "container", or "process"`)
	// ErrInvalidTimeout indicates timeout is negative.
	ErrInvalidTimeout = errors.New("timeout must not be negative")
	// ErrInvalidMemory indicates memory is negative.
	ErrInvalidMemory = errors.New("memory must not be negative")
	// ErrInvalidURLPath indicates url.path is set but does not start
	// with "/".
	ErrInvalidURLPath = errors.New(`url.path must start with "/"`)
)

// Manifest is the parsed and validated content of a function's
// `.lambda.yml` file. All fields are optional; unset fields are left at
// their zero value.
//
// Layering order: Load returns exactly what the file said, with no
// defaulting beyond parsing and validation. Callers that want inherited
// and built-in defaults applied run, in order:
//
//  1. Load — parse the manifest and validate it as written.
//  2. (*Config).ApplyDefaults — fill still-unset fields from the project's
//     lambdary.yml defaults block.
//  3. (*Manifest).ApplyBuiltinDefaults — fill any fields still unset with
//     Lambdary's own spec-level defaults (e.g. url.payload).
//
// Discovery (see internal/discovery) is expected to call them in that
// order, so a project-wide default always wins over a spec-level built-in.
type Manifest struct {
	// Name overrides the function name; defaults to the directory name
	// when unset.
	Name string `yaml:"name,omitempty"`
	// Runtime is an AWS Lambda runtime identifier (e.g. "nodejs22.x").
	Runtime string `yaml:"runtime,omitempty"`
	// Handler is the function handler, in the runtime's own notation
	// (e.g. "index.handler").
	Handler string `yaml:"handler,omitempty"`
	// Timeout is the invocation timeout in seconds. 0 means unset.
	Timeout int `yaml:"timeout,omitempty"`
	// Memory is the memory limit in MB, advisory locally. 0 means unset.
	Memory int `yaml:"memory,omitempty"`
	// Architectures lists target CPU architectures (e.g. "arm64").
	// Defaults to the host architecture when unset.
	Architectures []string `yaml:"architectures,omitempty"`
	// Layers lists layer version ARNs and/or local paths (a directory or
	// a .zip file, relative to the function directory), mirroring AWS
	// Lambda's own Layers configuration. At most 5 entries; they are
	// resolved and merged in declared order into the function's /opt,
	// with later entries winning when the same path appears in more than
	// one layer. See ParseLayerRef for how each entry is classified.
	Layers []string `yaml:"layers,omitempty"`
	// Environment holds environment variables passed to the function.
	Environment map[string]string `yaml:"environment,omitempty"`
	// URL configures the local Function URL route for this function.
	URL URL `yaml:"url,omitempty"`
	// Local holds Lambdary-specific, local-dev-only settings.
	Local Local `yaml:"local,omitempty"`
}

// URL configures how a function is exposed on the local HTTP router.
type URL struct {
	// Path is the route prefix; defaults to "/{name}" when unset.
	Path string `yaml:"path,omitempty"`
	// Payload is the event format used for the Function URL / API
	// Gateway mapping; defaults to DefaultPayload ("2.0") when unset, via
	// (*Manifest).ApplyBuiltinDefaults.
	Payload string `yaml:"payload,omitempty"`
}

// Local holds the one section of the manifest that goes beyond AWS's own
// configuration vocabulary: local-dev-only backend hints.
type Local struct {
	// Backend picks the execution backend: "auto" (default), "container",
	// or "process".
	Backend string `yaml:"backend,omitempty"`
	// Image overrides the container image used by the container backend.
	Image string `yaml:"image,omitempty"`
	// Command overrides the runtime binary used by the process backend.
	Command string `yaml:"command,omitempty"`
	// EnvFile names an env file loaded locally only (never deployed).
	EnvFile string `yaml:"env_file,omitempty"`
}

// Load reads and parses the `.lambda.yml` file at path, then validates it.
// It applies no defaulting beyond parsing: the returned Manifest reflects
// exactly what the file said. Unknown YAML keys are treated as errors so
// typos surface loudly. The returned error wraps the underlying cause with
// %w. See Manifest's doc comment for the intended defaulting layering.
func Load(path string) (*Manifest, error) {
	var m Manifest
	if err := decodeFile(path, &m); err != nil {
		return nil, fmt.Errorf("manifest: %s: %w", path, err)
	}

	if err := validateCommon(m.Timeout, m.Memory, m.URL, m.Local, m.Layers); err != nil {
		return nil, fmt.Errorf("manifest: %s: %w", path, err)
	}

	return &m, nil
}

// ApplyBuiltinDefaults fills any fields still unset on m with Lambdary's
// own spec-level defaults. It is the last step of the defaulting chain
// documented on Manifest, so it should run after Load and, if applicable,
// (*Config).ApplyDefaults — a project-wide default always wins over a
// built-in one. Keeping every spec-level default here, rather than in
// Load, keeps them in one place as more are added.
func (m *Manifest) ApplyBuiltinDefaults() {
	if m.URL.Payload == "" {
		m.URL.Payload = DefaultPayload
	}
}

// decodeFile opens path and decodes its YAML content into v, rejecting
// unknown fields. An empty file is treated as a valid, empty document.
func decodeFile(path string, v any) error {
	f, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open: %w", err)
	}
	defer f.Close()

	dec := yaml.NewDecoder(f)
	dec.KnownFields(true)

	if err := dec.Decode(v); err != nil && !errors.Is(err, io.EOF) {
		return fmt.Errorf("parse: %w", err)
	}

	return nil
}

// validateCommon applies the validation rules shared by Manifest and
// Config's defaults block: local.backend, timeout, memory, url.path, and
// layers. Callers add file-path and document-kind context to the returned
// error.
func validateCommon(timeout, memory int, url URL, local Local, layers []string) error {
	switch local.Backend {
	case "", "auto", "container", "process":
	default:
		return fmt.Errorf("%w: got %q", ErrInvalidBackend, local.Backend)
	}

	if timeout < 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidTimeout, timeout)
	}

	if memory < 0 {
		return fmt.Errorf("%w: got %d", ErrInvalidMemory, memory)
	}

	if url.Path != "" && !strings.HasPrefix(url.Path, "/") {
		return fmt.Errorf("%w: got %q", ErrInvalidURLPath, url.Path)
	}

	if err := validateLayers(layers); err != nil {
		return err
	}

	return nil
}

package manifest

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// DefaultPort is the local HTTP server port assumed when lambdary.yml
// omits port.
const DefaultPort = 8000

// DefaultRoot is the functions root folder assumed when lambdary.yml
// omits root.
const DefaultRoot = "."

// Errors returned by Config validation, wrapped with the config file path
// and the offending value.
var (
	// ErrInvalidPort indicates port is set outside the valid TCP port
	// range.
	ErrInvalidPort = errors.New("port must be between 1 and 65535")
	// ErrFunctionSource indicates a functions entry sets neither or both
	// of path and go.
	ErrFunctionSource = errors.New("exactly one of path or go must be set")
	// ErrFunctionName indicates a functions entry has an unusable name:
	// an empty key, a key with "/" or whitespace, or an inline name field
	// (the map key already is the function name).
	ErrFunctionName = errors.New("function name must be a non-empty map key without \"/\" or whitespace, and entries must not set name")
)

// Config is the parsed and validated content of a project's optional
// root-level lambdary.yml file.
type Config struct {
	// Port is the local HTTP server port. 0 means unset; defaults to
	// DefaultPort.
	Port int `yaml:"port,omitempty"`
	// Root is the functions root folder, relative to the config file.
	// Empty means unset; defaults to DefaultRoot.
	Root string `yaml:"root,omitempty"`
	// Defaults holds per-project defaults inherited by function
	// manifests that don't set the corresponding field.
	Defaults Defaults `yaml:"defaults,omitempty"`
	// Functions, when set, lists the project's functions explicitly by
	// name instead of discovering them from Root's subdirectories — for
	// layouts a directory scan can't express, such as one Go module
	// holding many cmd/<name> functions.
	Functions map[string]FunctionEntry `yaml:"functions,omitempty"`
	// Build holds project-wide build settings for compiled functions.
	Build Build `yaml:"build,omitempty"`
	// Queues declares local SQS queues by name, beyond those a function's
	// sqs events already create — for queues only producers and external
	// consumers use, or to set a consumed queue's attributes in one place.
	Queues map[string]QueueSettings `yaml:"queues,omitempty"`
}

// FunctionEntry is one function in Config.Functions. Exactly one of Path
// and Go is set; the remaining fields are the function's own manifest
// settings, written inline and taking precedence over a `.lambda.yml` in
// the function's directory.
type FunctionEntry struct {
	// Path is a function directory, relative to the functions root,
	// handled like a discovered one.
	Path string `yaml:"path,omitempty"`
	// Go is a Go main package directory, relative to the functions root,
	// built into the function's bootstrap.
	Go string `yaml:"go,omitempty"`
	// Manifest holds the entry's inline `.lambda.yml` settings.
	Manifest `yaml:",inline"`
}

// Build holds project-wide build settings for compiled functions.
type Build struct {
	// Go configures how Go functions are built.
	Go GoBuild `yaml:"go,omitempty"`
}

// GoBuild configures `go build` for Go functions.
type GoBuild struct {
	// Tags are build tags added after the lambda.norpc tag Lambdary
	// always sets.
	Tags []string `yaml:"tags,omitempty"`
	// LDFlags is passed to `go build -ldflags` as is.
	LDFlags string `yaml:"ldflags,omitempty"`
}

// Defaults mirrors Manifest's fields, minus Name: a function manifest has
// no meaningful project-wide default name.
type Defaults struct {
	// Runtime is the AWS Lambda runtime identifier inherited by
	// functions that don't set their own.
	Runtime string `yaml:"runtime,omitempty"`
	// Handler is the function handler inherited by functions that don't
	// set their own.
	Handler string `yaml:"handler,omitempty"`
	// Timeout is the invocation timeout in seconds. 0 means unset.
	Timeout int `yaml:"timeout,omitempty"`
	// Memory is the memory limit in MB. 0 means unset.
	Memory int `yaml:"memory,omitempty"`
	// Architectures lists target CPU architectures.
	Architectures []string `yaml:"architectures,omitempty"`
	// Layers lists layer version ARNs and/or local paths inherited by
	// functions that don't set their own.
	Layers []string `yaml:"layers,omitempty"`
	// Environment holds environment variables merged into functions
	// that don't set their own.
	Environment map[string]string `yaml:"environment,omitempty"`
	// URL configures the default local Function URL route settings.
	URL URL `yaml:"url,omitempty"`
	// Local holds default Lambdary-specific, local-dev-only settings.
	Local Local `yaml:"local,omitempty"`
}

// LoadConfig reads and parses the lambdary.yml file at path, validates it,
// then fills in Port and Root defaults. Unknown YAML keys are treated as
// errors. The returned error wraps the underlying cause with %w.
func LoadConfig(path string) (*Config, error) {
	var c Config
	if err := decodeFile(path, &c); err != nil {
		return nil, fmt.Errorf("config: %s: %w", path, err)
	}

	if err := c.validate(path); err != nil {
		return nil, err
	}

	if c.Port == 0 {
		c.Port = DefaultPort
	}

	if c.Root == "" {
		c.Root = DefaultRoot
	}

	return &c, nil
}

// validate checks Config-specific rules (port range) plus the rules
// shared with Manifest, applied to the defaults block.
func (c *Config) validate(path string) error {
	if c.Port < 0 || c.Port > 65535 {
		return fmt.Errorf("config: %s: %w: got %d", path, ErrInvalidPort, c.Port)
	}

	if err := validateCommon(c.Defaults.Timeout, c.Defaults.Memory, c.Defaults.URL, c.Defaults.Local, c.Defaults.Layers); err != nil {
		return fmt.Errorf("config: %s: defaults.%w", path, err)
	}

	if err := validateQueues(c.Queues); err != nil {
		return fmt.Errorf("config: %s: %w", path, err)
	}

	names := make([]string, 0, len(c.Functions))
	for name := range c.Functions {
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		if err := validateFunctionEntry(name, c.Functions[name]); err != nil {
			return fmt.Errorf("config: %s: functions.%s: %w", path, name, err)
		}
	}

	return nil
}

// validateFunctionEntry checks one Config.Functions entry: a usable name,
// exactly one source, and the inline manifest under the rules a
// `.lambda.yml` gets.
func validateFunctionEntry(name string, e FunctionEntry) error {
	if name == "" || strings.ContainsAny(name, "/ \t\r\n") || e.Name != "" {
		return ErrFunctionName
	}

	if (e.Path == "") == (e.Go == "") {
		return ErrFunctionSource
	}

	if err := validateCommon(e.Timeout, e.Memory, e.URL, e.Local, e.Layers); err != nil {
		return err
	}

	return validateEvents(e.Events)
}

// ApplyDefaults fills unset fields of m from c.Defaults. Fields already
// set on m are left untouched: the function manifest always wins over
// project-wide defaults. It is the middle step of the defaulting chain
// documented on Manifest: run it after Load and before
// (*Manifest).ApplyBuiltinDefaults, so a project-wide default wins over a
// spec-level built-in.
func (c *Config) ApplyDefaults(m *Manifest) {
	d := c.Defaults

	if m.Runtime == "" {
		m.Runtime = d.Runtime
	}
	if m.Handler == "" {
		m.Handler = d.Handler
	}
	if m.Timeout == 0 {
		m.Timeout = d.Timeout
	}
	if m.Memory == 0 {
		m.Memory = d.Memory
	}
	if len(m.Architectures) == 0 {
		m.Architectures = d.Architectures
	}
	if len(m.Layers) == 0 {
		m.Layers = d.Layers
	}
	if len(m.Environment) == 0 {
		m.Environment = d.Environment
	}
	if m.URL.Path == "" {
		m.URL.Path = d.URL.Path
	}
	if m.URL.Payload == "" {
		m.URL.Payload = d.URL.Payload
	}
	if m.URL.InvokeMode == "" {
		m.URL.InvokeMode = d.URL.InvokeMode
	}
	if m.Local.Backend == "" {
		m.Local.Backend = d.Local.Backend
	}
	if m.Local.Image == "" {
		m.Local.Image = d.Local.Image
	}
	if m.Local.Command == "" {
		m.Local.Command = d.Local.Command
	}
	if m.Local.EnvFile == "" {
		m.Local.EnvFile = d.Local.EnvFile
	}
}

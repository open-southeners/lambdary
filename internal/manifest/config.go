package manifest

import (
	"errors"
	"fmt"
)

// DefaultPort is the local HTTP server port assumed when lambdary.yml
// omits port.
const DefaultPort = 8000

// DefaultRoot is the functions root folder assumed when lambdary.yml
// omits root.
const DefaultRoot = "."

// ErrInvalidPort indicates port is set outside the valid TCP port range.
var ErrInvalidPort = errors.New("port must be between 1 and 65535")

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

	if err := validateCommon(c.Defaults.Timeout, c.Defaults.Memory, c.Defaults.URL, c.Defaults.Local); err != nil {
		return fmt.Errorf("config: %s: defaults.%w", path, err)
	}

	return nil
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
	if len(m.Environment) == 0 {
		m.Environment = d.Environment
	}
	if m.URL.Path == "" {
		m.URL.Path = d.URL.Path
	}
	if m.URL.Payload == "" {
		m.URL.Payload = d.URL.Payload
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

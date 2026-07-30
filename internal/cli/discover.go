package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"github.com/open-southeners/lambdary/internal/manifest"
)

// configFileName is the project-wide config file lambdary looks for at the
// --root directory, per DESIGN.md.
const configFileName = "lambdary.yml"

// resolveRoot resolves the scan root and optional project config for root,
// shared by every command that needs to run discovery (list, dev, invoke's
// standalone mode). An optional lambdary.yml directly under root can
// override the actual functions root (relative to root) and set
// project-wide port/defaults, per DESIGN.md's ".lambda.yml spec" section.
// cfg is nil when root has no lambdary.yml.
func resolveRoot(root string) (scanRoot string, cfg *manifest.Config, err error) {
	scanRoot = root

	cfgPath := filepath.Join(root, configFileName)

	if _, statErr := os.Stat(cfgPath); statErr == nil {
		loaded, err := manifest.LoadConfig(cfgPath)
		if err != nil {
			return "", nil, err
		}

		cfg = loaded
		scanRoot = filepath.Join(root, cfg.Root)
	} else if !os.IsNotExist(statErr) {
		return "", nil, fmt.Errorf("%s: %w", cfgPath, statErr)
	}

	return scanRoot, cfg, nil
}

// resolvePort resolves the local HTTP server port a command should use:
// flagPort when explicitly set (non-zero), else cfg's port (already
// defaulted by manifest.LoadConfig when cfg is non-nil), else
// manifest.DefaultPort.
func resolvePort(flagPort int, cfg *manifest.Config) int {
	if flagPort != 0 {
		return flagPort
	}

	if cfg != nil && cfg.Port != 0 {
		return cfg.Port
	}

	return manifest.DefaultPort
}

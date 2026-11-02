package container

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/open-southeners/lambdary/internal/backend"
)

// The settings DetectPlacement reads. Both are optional when Lambdary can
// inspect its own container.
const (
	// EnvNetwork names the Docker network function containers join.
	EnvNetwork = "LAMBDARY_NETWORK"
	// EnvHostRoot is the host path of the project's --root directory.
	EnvHostRoot = "LAMBDARY_HOST_ROOT"
)

// directPort is the emulator's port inside a function container: what a
// sibling container connects to, with no port published on the host.
const directPort = "8080"

// containerMarkers are files a container engine creates at a container's root.
var containerMarkers = []string{"/.dockerenv", "/run/.containerenv"}

// Placement says where Lambdary sits relative to the Docker daemon it drives.
// The zero value is the plain case: Lambdary on the daemon's host, so every
// path means the same to both, and a function is reached on a published port.
//
// Inside a container, the daemon still belongs to the host, which changes
// three things. Bind mount sources must be host paths (see HostPath). A
// function must share a network with Lambdary. And 127.0.0.1 is Lambdary's own
// loopback, so a function is reached at its container address (Direct).
type Placement struct {
	// Network is the network function containers join; "" means Docker's
	// default.
	Network string
	// Direct means Lambdary reaches functions at their address on Network
	// and publishes no ports.
	Direct bool

	// mounts maps Lambdary's paths to host paths, longest destination first.
	mounts []mount
	// strict makes a path outside every mount an error instead of passing it
	// through: in a container, such a path doesn't exist on the host.
	strict bool
}

type mount struct {
	source, destination string
}

// inspectResult is the part of `docker inspect` output Placement needs.
type inspectResult struct {
	Mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	} `json:"Mounts"`
	NetworkSettings struct {
		Networks map[string]struct {
			IPAddress string `json:"IPAddress"`
		} `json:"Networks"`
	} `json:"NetworkSettings"`
}

// DetectPlacement works out the Placement for a project at root, from the
// environment and, inside a container, from inspecting that container: its
// mounts give the path mapping and its network the default for functions.
func DetectPlacement(ctx context.Context, runner backend.Runner, cli, root string) (Placement, error) {
	return detectPlacement(ctx, runner, cli, root, os.Getenv, os.Hostname, inContainer())
}

func detectPlacement(
	ctx context.Context,
	runner backend.Runner,
	cli, root string,
	getenv func(string) string,
	hostname func() (string, error),
	contained bool,
) (Placement, error) {
	p := Placement{Network: getenv(EnvNetwork), Direct: contained, strict: contained}

	absRoot, err := filepath.Abs(root)
	if err != nil {
		return Placement{}, fmt.Errorf("container: resolving %s: %w", root, err)
	}

	if hostRoot := getenv(EnvHostRoot); hostRoot != "" {
		p.mounts = append(p.mounts, mount{source: hostRoot, destination: absRoot})
	}

	if contained {
		self, err := inspectSelf(ctx, runner, cli, hostname)

		switch {
		case err == nil:
			if err := p.adoptSelf(self); err != nil {
				return Placement{}, err
			}
		case p.Network == "" || len(p.mounts) == 0:
			// Without its own container's details, Lambdary can't fill in
			// whichever of these is missing.
			return Placement{}, fmt.Errorf("container: running in a container but could not inspect it, so set %s and %s: %w", EnvNetwork, EnvHostRoot, err)
		}
	}

	sort.SliceStable(p.mounts, func(i, j int) bool {
		return len(p.mounts[i].destination) > len(p.mounts[j].destination)
	})

	return p, nil
}

// adoptSelf fills p from Lambdary's own container. An explicit EnvHostRoot
// mount sorts ahead of an inspected one at the same destination, because
// sorting is stable and it was added first.
func (p *Placement) adoptSelf(self inspectResult) error {
	for _, m := range self.Mounts {
		p.mounts = append(p.mounts, mount{source: m.Source, destination: filepath.Clean(m.Destination)})
	}

	networks := make([]string, 0, len(self.NetworkSettings.Networks))
	for name := range self.NetworkSettings.Networks {
		networks = append(networks, name)
	}

	sort.Strings(networks)

	joined := strings.Join(networks, ", ")

	switch {
	case p.Network != "":
		if _, ok := self.NetworkSettings.Networks[p.Network]; !ok {
			return fmt.Errorf("container: %s is %q, but Lambdary's container is only on %s, so it could never reach those functions", EnvNetwork, p.Network, joined)
		}
	case len(networks) == 1:
		p.Network = networks[0]
	default:
		return fmt.Errorf("container: Lambdary's container is on several networks (%s), so set %s to the one functions should join", joined, EnvNetwork)
	}

	return nil
}

// HostPath translates one of Lambdary's paths into the path the Docker daemon
// knows it by.
func (p Placement) HostPath(path string) (string, error) {
	for _, m := range p.mounts {
		if path == m.destination {
			return m.source, nil
		}

		if rel, ok := strings.CutPrefix(path, m.destination+string(filepath.Separator)); ok {
			return filepath.Join(m.source, rel), nil
		}
	}

	if p.strict {
		return "", fmt.Errorf("%s isn't on a mount of Lambdary's container, so the host's Docker can't bind it: mount the project and run from inside the mount, or set %s", path, EnvHostRoot)
	}

	return path, nil
}

// place adapts runArgs' host-style argv to p: the publish flag makes way for
// --network, or stays beside it when Lambdary is on the host and reaches the
// function through the published port.
func (p Placement) place(args []string) []string {
	var extra []string
	if p.Network != "" {
		extra = []string{"--network", p.Network}
	}

	placed := make([]string, 0, len(args)+len(extra))

	for i := 0; i < len(args); i++ {
		if args[i] == "-p" && i+1 < len(args) && args[i+1] == publishSpec {
			if !p.Direct {
				placed = append(placed, args[i], args[i+1])
			}

			placed = append(placed, extra...)
			i++

			continue
		}

		placed = append(placed, args[i])
	}

	return placed
}

// address returns where Lambdary reaches container id.
func (b *containerBackend) address(ctx context.Context, id string) (host, port string, err error) {
	if !b.placement.Direct {
		port, err = resolvePort(ctx, b.runner, b.cli, id)
		return "127.0.0.1", port, err
	}

	info, err := inspect(ctx, b.runner, b.cli, id)
	if err != nil {
		return "", "", err
	}

	network, ok := info.NetworkSettings.Networks[b.placement.Network]
	if !ok || network.IPAddress == "" {
		return "", "", fmt.Errorf("container has no address on network %q; it may have exited at once", b.placement.Network)
	}

	return network.IPAddress, directPort, nil
}

func inspectSelf(ctx context.Context, runner backend.Runner, cli string, hostname func() (string, error)) (inspectResult, error) {
	name, err := hostname()
	if err != nil {
		return inspectResult{}, err
	}

	return inspect(ctx, runner, cli, name)
}

// inspect runs `<cli> inspect <name>`, which prints a one-element JSON array.
func inspect(ctx context.Context, runner backend.Runner, cli, name string) (inspectResult, error) {
	out, err := runner.Run(ctx, cli, "inspect", name)
	if err != nil {
		return inspectResult{}, fmt.Errorf("inspecting %s: %w", name, err)
	}

	var results []inspectResult
	if err := json.Unmarshal(out, &results); err != nil {
		return inspectResult{}, fmt.Errorf("inspecting %s: %w", name, err)
	}

	if len(results) == 0 {
		return inspectResult{}, errors.New("inspecting " + name + ": no such container")
	}

	return results[0], nil
}

func inContainer() bool {
	for _, marker := range containerMarkers {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}

	return false
}

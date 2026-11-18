package cli

import (
	"fmt"
	"io"
	"net"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/backend/container"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// awsFor builds the AWS environment backends inject from cfg: the zero value
// (inject nothing) unless lambdary.yml sets aws.environment. port is the dev
// server's port and host the address it listens on; standalone invoke has no
// server, so it passes 0 and "", which leaves AWS_ENDPOINT_URL out.
func awsFor(cfg *manifest.Config, port int, host string) backend.AWS {
	if cfg == nil || !cfg.AWS.Environment {
		return backend.AWS{}
	}

	return backend.AWS{
		Region:       cfg.AWS.RegionOrDefault(),
		Port:         port,
		LoopbackOnly: isLoopbackHost(host),
	}
}

// isLoopbackHost reports whether a listen host is reachable from this
// machine only. An empty host is not: it listens on every interface.
func isLoopbackHost(host string) bool {
	if host == "localhost" {
		return true
	}

	ip := net.ParseIP(host)

	return ip != nil && ip.IsLoopback()
}

// warnAWSLoopback tells the user, once per container backend resolved, that
// functions in containers can't reach a dev server listening on loopback
// only, when Lambdary runs on the Docker host and so reaches the dev server
// through the host gateway. Lambdary in a container is reached over the
// shared network, whatever address it listens on.
func warnAWSLoopback(errW io.Writer, aws backend.AWS, placement container.Placement) {
	if !aws.Enabled() || aws.Port <= 0 || !aws.LoopbackOnly || placement.Direct {
		return
	}

	fmt.Fprintf(errW, "warning: aws.environment points containers at the dev server on the host, but it listens on loopback only, so they can't reach it — start it with --host 0.0.0.0\n")
}

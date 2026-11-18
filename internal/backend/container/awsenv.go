package container

import "github.com/open-southeners/lambdary/internal/backend"

// hostGatewayName is the name a function container uses for the Docker host
// when Lambdary runs there, mapped by addHostArg.
const hostGatewayName = "host.docker.internal"

// addHostArg maps hostGatewayName to the host on Linux, where Docker has no
// such name of its own; Docker Desktop accepts it too.
const addHostArg = "--add-host=" + hostGatewayName + ":host-gateway"

// awsEndpointHost returns where a function container reaches Lambdary's dev
// server, and whether it needs addHostArg to resolve that name. Lambdary in
// a container is reached by its own container name on the shared network;
// Lambdary on the host through the Docker host. A host of "" means there is
// no address to give (no dev server, or the container's name is unknown).
func awsEndpointHost(aws backend.AWS, p Placement) (host string, addHost bool) {
	if !aws.Enabled() || aws.Port <= 0 {
		return "", false
	}

	if p.Direct {
		return p.SelfName, false
	}

	return hostGatewayName, true
}

// withAddHost inserts addHostArg into a `docker run` argv, right after the
// leading `run -d`.
func withAddHost(args []string) []string {
	const at = 2

	if len(args) < at {
		return append(args, addHostArg)
	}

	out := make([]string, 0, len(args)+1)
	out = append(out, args[:at]...)
	out = append(out, addHostArg)

	return append(out, args[at:]...)
}

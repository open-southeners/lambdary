package backend

import (
	"net"
	"strconv"
)

// AWSCredential is the placeholder value of the credential variables
// injected into functions: Lambdary's AWS-compatible API accepts any.
const AWSCredential = "lambdary"

// AWS describes the AWS environment backends inject into every function so
// an AWS SDK inside it talks to Lambdary. The zero value injects nothing.
type AWS struct {
	// Region is the region reported to functions. Empty disables the whole
	// injection.
	Region string
	// Port is the port of Lambdary's dev server. 0 means no dev server is
	// running, so no AWS_ENDPOINT_URL is injected.
	Port int
	// LoopbackOnly means the dev server listens on a loopback address only,
	// so a container can't reach it.
	LoopbackOnly bool
}

// Enabled reports whether any variable is injected.
func (a AWS) Enabled() bool {
	return a.Region != ""
}

// Vars returns the variables to inject. endpointHost is where the function
// reaches Lambdary's dev server; "" (or a zero Port) leaves out
// AWS_ENDPOINT_URL. Callers merge the result beneath the function's own
// environment so explicit configuration wins.
func (a AWS) Vars(endpointHost string) map[string]string {
	if !a.Enabled() {
		return nil
	}

	vars := map[string]string{
		"AWS_REGION":            a.Region,
		"AWS_DEFAULT_REGION":    a.Region,
		"AWS_ACCESS_KEY_ID":     AWSCredential,
		"AWS_SECRET_ACCESS_KEY": AWSCredential,
		"AWS_SESSION_TOKEN":     AWSCredential,
	}

	if a.Port > 0 && endpointHost != "" {
		vars["AWS_ENDPOINT_URL"] = "http://" + net.JoinHostPort(endpointHost, strconv.Itoa(a.Port))
	}

	return vars
}

// UnderEnv merges the injected variables beneath env (the function's
// env_file content), so every variable env sets wins. It returns env itself
// when nothing is injected.
func (a AWS) UnderEnv(env map[string]string, endpointHost string) map[string]string {
	vars := a.Vars(endpointHost)
	if len(vars) == 0 {
		return env
	}

	for k, v := range env {
		vars[k] = v
	}

	return vars
}

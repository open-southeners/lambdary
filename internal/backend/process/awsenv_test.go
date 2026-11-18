package process

import (
	"slices"
	"strings"
	"testing"

	"github.com/open-southeners/lambdary/internal/backend"
	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

func TestManifestEnvAWS(t *testing.T) {
	aws := backend.AWS{Region: "eu-south-2", Port: 9000}
	fn := discovery.Function{
		Name:     "hello",
		Manifest: &manifest.Manifest{Environment: map[string]string{"AWS_REGION": "mine"}},
	}

	env := manifestEnv(fn, aws.UnderEnv(map[string]string{"AWS_SESSION_TOKEN": "from-file"}, loopbackHost))

	for _, want := range []string{
		"AWS_REGION=mine", // the manifest beats the injection
		"AWS_DEFAULT_REGION=eu-south-2",
		"AWS_ACCESS_KEY_ID=lambdary",
		"AWS_SECRET_ACCESS_KEY=lambdary",
		"AWS_SESSION_TOKEN=from-file", // so does env_file
		"AWS_ENDPOINT_URL=http://127.0.0.1:9000",
	} {
		if !slices.Contains(env, want) {
			t.Errorf("env = %v, missing %q", env, want)
		}
	}

	if slices.Contains(env, "AWS_REGION=eu-south-2") {
		t.Errorf("env = %v, injected region overrode the manifest's", env)
	}
}

func TestManifestEnvAWSDisabled(t *testing.T) {
	fn := discovery.Function{Name: "hello"}

	got := manifestEnv(fn, backend.AWS{}.UnderEnv(nil, loopbackHost))

	for _, e := range got {
		if strings.HasPrefix(e, "AWS_") && !strings.HasPrefix(e, "AWS_LAMBDA_") {
			t.Errorf("env has %q with the injection disabled", e)
		}
	}
}

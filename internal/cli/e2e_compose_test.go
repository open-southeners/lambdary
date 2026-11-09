package cli

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

const (
	composeE2EImage = "lambdary:compose-e2e"
	composeE2EEnv   = "LAMBDARY_E2E_COMPOSE"
)

// composeE2EFile is the stack under test: the official image, with the source
// mounted at a path that doesn't exist on the host and running as the host user,
// next to a client and a peer that other services would be in real life.
const composeE2EFile = `services:
  lambdary:
    image: %[1]s
    user: "%[2]d:%[3]d"
    group_add: ["%[4]d"]
    stop_grace_period: 40s
    volumes:
      - /var/run/docker.sock:/var/run/docker.sock
      - %[5]s:/workspace
  peer:
    image: busybox:stable
    command: ["sh", "-c", "mkdir -p /www && echo peer-answer > /www/index.html && exec httpd -f -p 8080 -h /www"]
  client:
    image: busybox:stable
    command: ["sleep", "3600"]
`

const composeE2EConfig = `functions:
  e2e-go:
    go: ./cmd/hello
  e2e-py:
    path: ./py
`

const composeE2EPython = `import urllib.request


def handler(event, context):
    peer = urllib.request.urlopen("http://peer:8080/", timeout=5).read().decode().strip()
    return {"statusCode": 200, "body": "py says v1, peer says " + peer}
`

const composeE2EPythonManifest = `runtime: python3.13
handler: lambda_function.handler
`

// TestE2ECompose proves the feature's done-when: a Compose service with the
// Docker socket and the source mounted serves functions reachable from other
// services by name. It builds the image, so it only runs when asked:
//
//	LAMBDARY_E2E_COMPOSE=1 go test ./internal/cli/ -run TestE2ECompose
func TestE2ECompose(t *testing.T) {
	if os.Getenv(composeE2EEnv) == "" {
		t.Skipf("set %s=1 to build the image and run this test", composeE2EEnv)
	}

	var socket syscall.Stat_t
	if err := syscall.Stat("/var/run/docker.sock", &socket); err != nil {
		t.Skipf("no docker socket: %v", err)
	}

	mustRun(t, 10*time.Minute, "docker", "build", "-q", "-t", composeE2EImage, "../..")

	root := composeE2ERoot(t)

	stackDir := t.TempDir()
	stackFile := filepath.Join(stackDir, "docker-compose.yml")

	stack := fmt.Sprintf(composeE2EFile, composeE2EImage, os.Getuid(), os.Getgid(), socket.Gid, root)
	if err := os.WriteFile(stackFile, []byte(stack), 0o644); err != nil {
		t.Fatalf("writing %s: %v", stackFile, err)
	}

	compose := composeStack{t: t, project: fmt.Sprintf("lambdary-e2e-%d", os.Getpid()), file: stackFile}

	t.Cleanup(func() {
		if t.Failed() {
			t.Logf("lambdary logs:\n%s", compose.tryRun("logs", "--no-color", "lambdary"))
		}

		compose.tryRun("down", "--volumes", "--timeout", "40")
		stopFunctionContainers(t)
	})

	compose.run("up", "-d", "--wait")

	// A service reaches Lambdary by name, and a function reaches a service by
	// name. The function's code came through a translated host path.
	compose.waitFor("GET", "http://lambdary:8000/e2e-py", "", "py says v1, peer says peer-answer")

	// A Go function builds inside the container, with no toolchain from the
	// host and nothing built by hand.
	compose.waitFor("GET", "http://lambdary:8000/e2e-go", "", "hello says v1")

	// Function containers publish nothing and share the Compose network.
	details := mustRun(t, time.Minute, "docker", "ps", "--filter", "label=lambdary.function=e2e-py", "--format", "{{.Networks}} {{.Ports}}")
	if !strings.Contains(details, compose.project+"_default") {
		t.Errorf("function container's networks and ports = %q, want the %s_default network", details, compose.project)
	}
	if strings.Contains(details, "->") {
		t.Errorf("function container's networks and ports = %q, want no published port", details)
	}

	// Hot reload sees edits made on the host through the bind mount.
	editPython(t, root, "v1", "v2")
	compose.waitFor("GET", "http://lambdary:8000/e2e-py", "", "py says v2")

	// A published version's snapshot lives under .lambdary, on another
	// translated path, and stays put when $LATEST moves on.
	compose.waitFor("POST", "http://lambdary:8000/2015-03-31/functions/e2e-py/versions", "{}", `"Version":"1"`)

	editPython(t, root, "v2", "v3")
	compose.waitFor("GET", "http://lambdary:8000/e2e-py", "", "py says v3")
	compose.waitFor("POST", "http://lambdary:8000/2015-03-31/functions/e2e-py/invocations?Qualifier=1", "{}", "py says v2")

	// The non-root user keeps the source mount in the host user's hands.
	var cache syscall.Stat_t
	if err := syscall.Stat(filepath.Join(root, ".lambdary"), &cache); err != nil {
		t.Fatalf("stat .lambdary: %v", err)
	}
	if int(cache.Uid) != os.Getuid() {
		t.Errorf(".lambdary is owned by uid %d, want %d", cache.Uid, os.Getuid())
	}

	// A clean stop takes every function container with it, versions included.
	compose.run("stop", "lambdary")

	for _, fn := range []string{"e2e-py", "e2e-go"} {
		if left := mustRun(t, time.Minute, "docker", "ps", "-q", "--filter", "label=lambdary.function="+fn); strings.TrimSpace(left) != "" {
			t.Errorf("%s still has containers after stopping Lambdary: %s", fn, left)
		}
	}
}

// composeE2ERoot lays out the project: the Go module fixture plus a Python
// function that calls a peer service.
func composeE2ERoot(t *testing.T) string {
	t.Helper()

	root := t.TempDir()

	if err := os.CopyFS(root, os.DirFS("testdata/go-module")); err != nil {
		t.Fatalf("copying fixture: %v", err)
	}

	files := map[string]string{
		"lambdary.yml":          composeE2EConfig,
		"py/lambda_function.py": composeE2EPython,
		"py/.lambda.yml":        composeE2EPythonManifest,
	}

	for name, content := range files {
		path := filepath.Join(root, name)

		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatalf("creating %s: %v", filepath.Dir(path), err)
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	return root
}

func editPython(t *testing.T, root, from, to string) {
	t.Helper()

	path := filepath.Join(root, "py", "lambda_function.py")

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("reading %s: %v", path, err)
	}

	edited := strings.Replace(string(data), "py says "+from, "py says "+to, 1)
	if edited == string(data) {
		t.Fatalf("%s has no %q to replace", path, from)
	}

	if err := os.WriteFile(path, []byte(edited), 0o644); err != nil {
		t.Fatalf("writing %s: %v", path, err)
	}
}

// stopFunctionContainers removes what a failed test may have left running.
func stopFunctionContainers(t *testing.T) {
	t.Helper()

	for _, fn := range []string{"e2e-py", "e2e-go"} {
		ids, err := exec.Command("docker", "ps", "-q", "--filter", "label=lambdary.function="+fn).Output()
		if err != nil {
			continue
		}

		for _, id := range strings.Fields(string(ids)) {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	}
}

// composeStack runs docker compose against one project.
type composeStack struct {
	t       *testing.T
	project string
	file    string
}

func (c composeStack) args(args ...string) []string {
	return append([]string{"compose", "-p", c.project, "-f", c.file}, args...)
}

func (c composeStack) run(args ...string) string {
	c.t.Helper()

	return mustRun(c.t, 5*time.Minute, "docker", c.args(args...)...)
}

func (c composeStack) tryRun(args ...string) string {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	out, _ := exec.CommandContext(ctx, "docker", c.args(args...)...).CombinedOutput()

	return string(out)
}

// waitFor makes the request from the client service until the answer contains
// want, covering image pulls, builds and hot reload's short delay.
func (c composeStack) waitFor(method, url, body, want string) {
	c.t.Helper()

	args := []string{"exec", "-T", "client", "wget", "-qO-", "-T", "60"}
	if method == "POST" {
		args = append(args, "--post-data", body)
	}

	args = append(args, url)

	deadline := time.Now().Add(4 * time.Minute)

	var last string

	for time.Now().Before(deadline) {
		last = c.tryRun(args...)
		if strings.Contains(last, want) {
			return
		}

		time.Sleep(time.Second)
	}

	c.t.Fatalf("%s %s from the client never answered with %q; last answer: %s", method, url, want, last)
}

func mustRun(t *testing.T, timeout time.Duration, name string, args ...string) string {
	t.Helper()

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v: %v\n%s", name, args, err, out)
	}

	return string(out)
}

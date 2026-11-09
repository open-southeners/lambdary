package container

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/open-southeners/lambdary/internal/discovery"
	"github.com/open-southeners/lambdary/internal/manifest"
)

// envMap returns a getenv backed by m.
func envMap(m map[string]string) func(string) string {
	return func(k string) string { return m[k] }
}

func hostnameOf(name string) func() (string, error) {
	return func() (string, error) { return name, nil }
}

// inspectJSON builds Docker-shaped `inspect` output: a one-element array.
func inspectJSON(mounts [][2]string, networks map[string]string) string {
	var sb strings.Builder

	sb.WriteString(`[{"Mounts":[`)

	for i, m := range mounts {
		if i > 0 {
			sb.WriteString(",")
		}

		sb.WriteString(`{"Type":"bind","Source":"` + m[0] + `","Destination":"` + m[1] + `"}`)
	}

	sb.WriteString(`],"NetworkSettings":{"Networks":{`)

	first := true

	for name, ip := range networks {
		if !first {
			sb.WriteString(",")
		}

		first = false

		sb.WriteString(`"` + name + `":{"IPAddress":"` + ip + `"}`)
	}

	sb.WriteString(`}}}]`)

	return sb.String()
}

func inspectRunner(out string, err error) *fakeRunner {
	return &fakeRunner{respond: map[string]func(args []string) (string, error){
		"inspect": func(args []string) (string, error) { return out, err },
	}}
}

func mustHostPath(t *testing.T, p Placement, path string) string {
	t.Helper()

	got, err := p.HostPath(path)
	if err != nil {
		t.Fatalf("HostPath(%q) unexpected error: %v", path, err)
	}

	return got
}

func TestDetectPlacementHost(t *testing.T) {
	t.Run("no settings: zero behaviour, no inspect", func(t *testing.T) {
		r := &fakeRunner{}

		p, err := detectPlacement(context.Background(), r, "docker", "/proj", envMap(nil), hostnameOf("h"), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if p.Network != "" || p.Direct {
			t.Errorf("placement = %+v, want zero behaviour", p)
		}

		if n := len(r.callsFor("inspect")); n != 0 {
			t.Errorf("inspect calls = %d, want 0", n)
		}

		if got := mustHostPath(t, p, "/anything/else"); got != "/anything/else" {
			t.Errorf("HostPath = %q, want identity", got)
		}

		args := []string{"run", "-d", "-p", publishSpec, "-v", "/a:/var/task:ro", "img"}
		if got := p.place(args); !reflect.DeepEqual(got, args) {
			t.Errorf("place() = %v, want unchanged %v", got, args)
		}
	})

	t.Run("LAMBDARY_NETWORK adds --network and keeps -p", func(t *testing.T) {
		r := &fakeRunner{}

		p, err := detectPlacement(context.Background(), r, "docker", "/proj", envMap(map[string]string{EnvNetwork: "devmesh"}), hostnameOf("h"), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if p.Network != "devmesh" || p.Direct {
			t.Fatalf("placement = %+v, want network devmesh, not direct", p)
		}

		if n := len(r.callsFor("inspect")); n != 0 {
			t.Errorf("inspect calls = %d, want 0", n)
		}

		got := p.place([]string{"run", "-p", publishSpec, "img"})
		want := []string{"run", "-p", publishSpec, "--network", "devmesh", "img"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("place() = %v, want %v", got, want)
		}
	})

	t.Run("LAMBDARY_HOST_ROOT translates root, others pass through", func(t *testing.T) {
		p, err := detectPlacement(context.Background(), &fakeRunner{}, "docker", "/proj", envMap(map[string]string{EnvHostRoot: "/host/proj"}), hostnameOf("h"), false)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got := mustHostPath(t, p, "/proj"); got != "/host/proj" {
			t.Errorf("HostPath(/proj) = %q, want /host/proj", got)
		}

		if got := mustHostPath(t, p, "/proj/fn/a"); got != "/host/proj/fn/a" {
			t.Errorf("HostPath(/proj/fn/a) = %q, want /host/proj/fn/a", got)
		}

		if got := mustHostPath(t, p, "/elsewhere/x"); got != "/elsewhere/x" {
			t.Errorf("HostPath(/elsewhere/x) = %q, want passthrough", got)
		}
	})
}

func TestDetectPlacementInContainer(t *testing.T) {
	ctx := context.Background()

	t.Run("mounts translate; longest destination wins; sibling prefix doesn't match; one network adopted; -p dropped", func(t *testing.T) {
		out := inspectJSON(
			[][2]string{
				{"/srv/work", "/workspace"},
				{"/srv/other/vendor", "/workspace/vendor"},
			},
			map[string]string{"compose_default": "172.20.0.2"},
		)
		r := inspectRunner(out, nil)

		p, err := detectPlacement(ctx, r, "docker", "/workspace", envMap(nil), hostnameOf("abc123"), true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if calls := r.callsFor("inspect"); len(calls) != 1 || !reflect.DeepEqual(calls[0], []string{"inspect", "abc123"}) {
			t.Errorf("inspect calls = %v, want one for hostname abc123", calls)
		}

		if p.Network != "compose_default" || !p.Direct {
			t.Errorf("placement = %+v, want network compose_default, direct", p)
		}

		cases := map[string]string{
			"/workspace":              "/srv/work",
			"/workspace/fn/a":         "/srv/work/fn/a",
			"/workspace/vendor":       "/srv/other/vendor",
			"/workspace/vendor/x/y":   "/srv/other/vendor/x/y",
			"/workspace/vendorish/zz": "/srv/work/vendorish/zz",
		}

		for in, want := range cases {
			if got := mustHostPath(t, p, in); got != want {
				t.Errorf("HostPath(%q) = %q, want %q", in, got, want)
			}
		}

		if _, err := p.HostPath("/workspace2/fn"); err == nil {
			t.Error("HostPath(/workspace2/fn) succeeded; a sibling prefix must not match /workspace")
		}

		got := p.place([]string{"run", "-p", publishSpec, "img"})
		want := []string{"run", "--network", "compose_default", "img"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("place() = %v, want %v", got, want)
		}
	})

	t.Run("LAMBDARY_HOST_ROOT beats an inspected mount at the same destination", func(t *testing.T) {
		out := inspectJSON([][2]string{{"/inspected", "/workspace"}}, map[string]string{"n": "10.0.0.2"})

		p, err := detectPlacement(ctx, inspectRunner(out, nil), "docker", "/workspace", envMap(map[string]string{EnvHostRoot: "/explicit"}), hostnameOf("h"), true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if got := mustHostPath(t, p, "/workspace/fn"); got != "/explicit/fn" {
			t.Errorf("HostPath = %q, want /explicit/fn", got)
		}
	})

	t.Run("LAMBDARY_NETWORK naming a network it isn't on errors naming both", func(t *testing.T) {
		out := inspectJSON(nil, map[string]string{"alpha": "10.0.0.2"})

		_, err := detectPlacement(ctx, inspectRunner(out, nil), "docker", "/workspace", envMap(map[string]string{EnvNetwork: "beta"}), hostnameOf("h"), true)
		if err == nil {
			t.Fatal("expected an error")
		}

		for _, want := range []string{"beta", "alpha"} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	})

	t.Run("several networks and no setting errors naming them", func(t *testing.T) {
		out := inspectJSON(nil, map[string]string{"alpha": "10.0.0.2", "beta": "10.0.1.2"})

		_, err := detectPlacement(ctx, inspectRunner(out, nil), "docker", "/workspace", envMap(nil), hostnameOf("h"), true)
		if err == nil {
			t.Fatal("expected an error")
		}

		for _, want := range []string{"alpha", "beta", EnvNetwork} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("error %q should mention %q", err, want)
			}
		}
	})

	t.Run("several networks with a setting picks it", func(t *testing.T) {
		out := inspectJSON(nil, map[string]string{"alpha": "10.0.0.2", "beta": "10.0.1.2"})

		p, err := detectPlacement(ctx, inspectRunner(out, nil), "docker", "/workspace", envMap(map[string]string{EnvNetwork: "beta"}), hostnameOf("h"), true)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}

		if p.Network != "beta" {
			t.Errorf("Network = %q, want beta", p.Network)
		}
	})

	t.Run("inspect failing errors mentioning both vars unless both are explicit", func(t *testing.T) {
		fail := errors.New("no docker socket")

		partial := []map[string]string{
			nil,
			{EnvNetwork: "n"},
			{EnvHostRoot: "/h"},
		}

		for _, env := range partial {
			_, err := detectPlacement(ctx, inspectRunner("", fail), "docker", "/workspace", envMap(env), hostnameOf("h"), true)
			if err == nil {
				t.Fatalf("env %v: expected an error", env)
			}

			for _, want := range []string{EnvNetwork, EnvHostRoot} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("env %v: error %q should mention %s", env, err, want)
				}
			}
		}

		p, err := detectPlacement(ctx, inspectRunner("", fail), "docker", "/workspace", envMap(map[string]string{EnvNetwork: "n", EnvHostRoot: "/h"}), hostnameOf("h"), true)
		if err != nil {
			t.Fatalf("both explicit: unexpected error: %v", err)
		}

		if p.Network != "n" || !p.Direct {
			t.Errorf("placement = %+v, want network n, direct", p)
		}

		if got := mustHostPath(t, p, "/workspace/fn"); got != "/h/fn" {
			t.Errorf("HostPath = %q, want /h/fn", got)
		}
	})
}

func TestHostPathInContainerOutsideMounts(t *testing.T) {
	out := inspectJSON([][2]string{{"/srv/work", "/workspace"}}, map[string]string{"n": "10.0.0.2"})

	p, err := detectPlacement(context.Background(), inspectRunner(out, nil), "docker", "/workspace", envMap(nil), hostnameOf("h"), true)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	_, err = p.HostPath("/tmp/elsewhere")
	if err == nil {
		t.Fatal("expected an error for a path outside every mount")
	}

	for _, want := range []string{"/tmp/elsewhere", EnvHostRoot} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	if got := mustHostPath(t, p, "/workspace"); got != "/srv/work" {
		t.Errorf("HostPath of the exact mount destination = %q, want /srv/work", got)
	}
}

// listenDirectPort serves the readiness probe on 127.0.0.1:8080, the fixed
// port Direct mode connects to. It reports false when the port is taken.
func listenDirectPort(t *testing.T) bool {
	t.Helper()

	l, err := net.Listen("tcp", "127.0.0.1:"+directPort)
	if err != nil {
		return false
	}

	srv := httptest.NewUnstartedServer(http.NotFoundHandler())
	srv.Listener.Close()
	srv.Listener = l
	srv.Start()
	t.Cleanup(srv.Close)

	return true
}

func putFile(t *testing.T, path, content string) {
	t.Helper()

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

// volumeArgs returns the values following every -v in argv.
func volumeArgs(argv []string) []string {
	var vols []string

	for i := 0; i < len(argv)-1; i++ {
		if argv[i] == "-v" {
			vols = append(vols, argv[i+1])
		}
	}

	return vols
}

func contains(list []string, want string) bool {
	for _, s := range list {
		if s == want {
			return true
		}
	}

	return false
}

func TestStartTranslatesBindSources(t *testing.T) {
	root := t.TempDir()
	fnDir := filepath.Join(root, "fn")
	layerDir := filepath.Join(root, "layer")
	cacheDir := filepath.Join(root, ".lambdary")

	putFile(t, filepath.Join(fnDir, "bootstrap"), "#!/bin/sh\n")
	putFile(t, filepath.Join(layerDir, "lib", "x"), "x")

	hostPort := listenLoopback(t)

	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"run":  func(args []string) (string, error) { return "cid\n", nil },
		"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
		"stop": func(args []string) (string, error) { return "", nil },
	}}

	b := &containerBackend{
		cli: "docker", runner: r, readyTimeout: 2 * time.Second, cacheDir: cacheDir,
		placement: Placement{
			Network: "devmesh",
			mounts:  []mount{{source: "/host/proj", destination: root}},
		},
	}

	fn := discovery.Function{
		Name:     "hello",
		Dir:      fnDir,
		Runtime:  "provided.al2023",
		Manifest: &manifest.Manifest{Layers: []string{layerDir}},
	}

	inst, err := b.Start(context.Background(), fn)
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer inst.Stop(context.Background())

	runs := r.callsFor("run")
	if len(runs) != 1 {
		t.Fatalf("run calls = %d, want 1", len(runs))
	}

	vols := volumeArgs(runs[0])

	for _, want := range []string{
		"/host/proj/fn:/var/task:ro",
		"/host/proj/.lambdary/staging/hello:/opt:ro",
		"/host/proj/fn/bootstrap:/var/runtime/bootstrap:ro",
	} {
		if !contains(vols, want) {
			t.Errorf("run volumes = %v, missing %q", vols, want)
		}
	}

	for _, v := range vols {
		if strings.HasPrefix(v, root) {
			t.Errorf("volume %q still uses Lambdary's own path", v)
		}
	}

	// Host publishing stays beside --network, so the published port is used.
	if !contains(runs[0], "-p") || !contains(runs[0], "--network") {
		t.Errorf("run argv = %v, want both -p and --network", runs[0])
	}

	if got := inst.InvokeURL(); !strings.HasPrefix(got, "http://127.0.0.1:"+hostPort+"/") {
		t.Errorf("InvokeURL() = %q, want loopback published port", got)
	}
}

func TestStartDockerfileBuildKeepsContainerPath(t *testing.T) {
	root := t.TempDir()
	fnDir := filepath.Join(root, "fn")
	if err := os.MkdirAll(fnDir, 0o755); err != nil {
		t.Fatal(err)
	}

	writeDockerfile(t, fnDir)

	hostPort := listenLoopback(t)

	r := &fakeRunner{respond: map[string]func(args []string) (string, error){
		"run":  func(args []string) (string, error) { return "cid\n", nil },
		"port": func(args []string) (string, error) { return "127.0.0.1:" + hostPort + "\n", nil },
		"stop": func(args []string) (string, error) { return "", nil },
	}}

	b := &containerBackend{
		cli: "docker", runner: r, readyTimeout: 2 * time.Second,
		placement: Placement{mounts: []mount{{source: "/host/proj", destination: root}}},
	}

	fn := discovery.Function{Name: "img", Dir: fnDir, Backend: "container"}

	inst, err := b.Start(context.Background(), fn)
	if err != nil {
		t.Fatalf("Start() unexpected error: %v", err)
	}
	defer inst.Stop(context.Background())

	builds := r.callsFor("build")
	if len(builds) != 1 {
		t.Fatalf("build calls = %d, want 1", len(builds))
	}

	if last := builds[0][len(builds[0])-1]; last != fnDir {
		t.Errorf("build context = %q, want container path %q", last, fnDir)
	}

	runs := r.callsFor("run")
	if len(runs) != 1 || !contains(volumeArgs(runs[0]), "/host/proj/fn:/var/task:ro") {
		t.Errorf("run argv = %v, want /var/task bound from the host path", runs)
	}
}

func TestStartDirect(t *testing.T) {
	root := t.TempDir()
	fnDir := filepath.Join(root, "fn")
	putFile(t, filepath.Join(fnDir, "h.py"), "")

	fn := discovery.Function{Name: "hello", Dir: fnDir, Handler: "h.handler", Runtime: "python3.13"}

	newBackend := func(r *fakeRunner) *containerBackend {
		return &containerBackend{
			cli: "docker", runner: r, readyTimeout: 500 * time.Millisecond,
			placement: Placement{
				Network: "compose_default", Direct: true, strict: true,
				mounts: []mount{{source: "/host/proj", destination: root}},
			},
		}
	}

	t.Run("no port call and no -p; InvokeURL uses the container IP and 8080", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run": func(args []string) (string, error) { return "cid\n", nil },
			"inspect": func(args []string) (string, error) {
				return inspectJSON(nil, map[string]string{"compose_default": "127.0.0.1"}), nil
			},
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		served := listenDirectPort(t)

		inst, err := newBackend(r).Start(context.Background(), fn)

		if n := len(r.callsFor("port")); n != 0 {
			t.Errorf("port calls = %d, want 0", n)
		}

		runs := r.callsFor("run")
		if len(runs) != 1 {
			t.Fatalf("run calls = %d, want 1", len(runs))
		}

		if contains(runs[0], "-p") {
			t.Errorf("run argv = %v, must not publish a port", runs[0])
		}

		if !contains(runs[0], "--network") || !contains(runs[0], "compose_default") {
			t.Errorf("run argv = %v, want --network compose_default", runs[0])
		}

		if !served {
			// 127.0.0.1:8080 is taken here, so readiness can't be satisfied.
			if err == nil {
				inst.Stop(context.Background())
				return
			}

			if !strings.Contains(err.Error(), "127.0.0.1:8080") {
				t.Errorf("Start() error = %v, want readiness failure against 127.0.0.1:8080", err)
			}

			return
		}

		if err != nil {
			t.Fatalf("Start() unexpected error: %v", err)
		}
		defer inst.Stop(context.Background())

		want := "http://127.0.0.1:8080/2015-03-31/functions/function/invocations"
		if got := inst.InvokeURL(); got != want {
			t.Errorf("InvokeURL() = %q, want %q", got, want)
		}
	})

	t.Run("no address on the network: errors, stops the container, no port call", func(t *testing.T) {
		r := &fakeRunner{respond: map[string]func(args []string) (string, error){
			"run": func(args []string) (string, error) { return "cid\n", nil },
			"inspect": func(args []string) (string, error) {
				return inspectJSON(nil, map[string]string{"other": "10.0.0.9"}), nil
			},
			"stop": func(args []string) (string, error) { return "", nil },
		}}

		if _, err := newBackend(r).Start(context.Background(), fn); err == nil {
			t.Fatal("Start() expected an error")
		} else if !strings.Contains(err.Error(), "compose_default") {
			t.Errorf("error %q should name the network", err)
		}

		stops := r.callsFor("stop")
		if len(stops) != 1 || stops[0][len(stops[0])-1] != "cid" {
			t.Errorf("stop calls = %v, want one for cid", stops)
		}

		if n := len(r.callsFor("port")); n != 0 {
			t.Errorf("port calls = %d, want 0", n)
		}
	})
}

func TestStartBindSourceOutsideMounts(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	putFile(t, filepath.Join(outside, "h.py"), "")

	r := &fakeRunner{}

	b := &containerBackend{
		cli: "docker", runner: r, readyTimeout: 200 * time.Millisecond,
		placement: Placement{
			Network: "n", Direct: true, strict: true,
			mounts: []mount{{source: "/host/proj", destination: root}},
		},
	}

	fn := discovery.Function{Name: "hello", Dir: outside, Handler: "h.handler", Runtime: "python3.13"}

	_, err := b.Start(context.Background(), fn)
	if err == nil {
		t.Fatal("Start() expected an error")
	}

	for _, want := range []string{outside, EnvHostRoot} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q should mention %q", err, want)
		}
	}

	if n := len(r.callsFor("run")); n != 0 {
		t.Errorf("run calls = %d, want 0", n)
	}
}

func TestInstanceInvokeURLEmptyHost(t *testing.T) {
	i := &instance{port: "4321"}

	want := "http://127.0.0.1:4321/2015-03-31/functions/function/invocations"
	if got := i.InvokeURL(); got != want {
		t.Errorf("InvokeURL() = %q, want %q", got, want)
	}

	i.host = "10.1.2.3"
	if got := i.InvokeURL(); !strings.HasPrefix(got, "http://10.1.2.3:4321/") {
		t.Errorf("InvokeURL() = %q, want host 10.1.2.3", got)
	}
}

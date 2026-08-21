package process

import (
	"os"
	"reflect"
	"testing"

	"github.com/open-southeners/lambdary/internal/discovery"
)

func TestUpsertEnvPath(t *testing.T) {
	sep := string(os.PathListSeparator)

	t.Run("key absent: appends a new entry", func(t *testing.T) {
		got := upsertEnvPath([]string{"OTHER=x"}, "NODE_PATH", "/staging/nodejs/node_modules")
		want := []string{"OTHER=x", "NODE_PATH=/staging/nodejs/node_modules"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("upsertEnvPath() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("key present with a non-empty value: appends after it with the OS separator, host value kept first", func(t *testing.T) {
		got := upsertEnvPath([]string{"PATH=/usr/bin"}, "PATH", "/staging/bin")
		want := []string{"PATH=/usr/bin" + sep + "/staging/bin"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("upsertEnvPath() =\n%v\nwant\n%v (host value must stay first)", got, want)
		}
	})

	t.Run("key present with an empty value: no leading separator", func(t *testing.T) {
		got := upsertEnvPath([]string{"NODE_PATH="}, "NODE_PATH", "/staging/nodejs/node_modules")
		want := []string{"NODE_PATH=/staging/nodejs/node_modules"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("upsertEnvPath() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("multiple values are joined with the OS separator, in order", func(t *testing.T) {
		got := upsertEnvPath(nil, "NODE_PATH", "/a", "/b")
		want := []string{"NODE_PATH=/a" + sep + "/b"}

		if !reflect.DeepEqual(got, want) {
			t.Errorf("upsertEnvPath() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("no values is a no-op", func(t *testing.T) {
		env := []string{"FOO=bar"}
		got := upsertEnvPath(env, "NODE_PATH")

		if !reflect.DeepEqual(got, env) {
			t.Errorf("upsertEnvPath() = %v, want the env unchanged", got)
		}
	})

	t.Run("never introduces a duplicate key", func(t *testing.T) {
		got := upsertEnvPath([]string{"A=1", "NODE_PATH=/x", "B=2"}, "NODE_PATH", "/y")

		count := 0
		for _, kv := range got {
			if len(kv) >= len("NODE_PATH=") && kv[:len("NODE_PATH=")] == "NODE_PATH=" {
				count++
			}
		}
		if count != 1 {
			t.Errorf("upsertEnvPath() produced %d NODE_PATH entries in %v, want exactly 1", count, got)
		}

		want := []string{"A=1", "NODE_PATH=/x" + sep + "/y", "B=2"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("upsertEnvPath() =\n%v\nwant\n%v", got, want)
		}
	})
}

func TestLayerSearchPathEnv(t *testing.T) {
	const staging = "/cache/staging/hello"

	t.Run("nodejs: NODE_PATH gets both entries, PATH gets staging/bin", func(t *testing.T) {
		fn := discovery.Function{Runtime: "nodejs22.x"}

		got := layerSearchPathEnv(nil, fn, staging)

		wantNode := "NODE_PATH=/cache/staging/hello/nodejs/node_modules" + string(os.PathListSeparator) + "/cache/staging/hello/nodejs/node22/node_modules"
		wantPath := "PATH=/cache/staging/hello/bin"

		if !contains(got, wantNode) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, wantNode)
		}
		if !contains(got, wantPath) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, wantPath)
		}
	})

	t.Run("nodejs runtime id that doesn't parse a major version still gets the version-independent NODE_PATH entry", func(t *testing.T) {
		fn := discovery.Function{Runtime: "nodejs"}

		got := layerSearchPathEnv(nil, fn, staging)

		want := "NODE_PATH=/cache/staging/hello/nodejs/node_modules"
		if !contains(got, want) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, want)
		}
	})

	t.Run("python: PYTHONPATH gets both entries", func(t *testing.T) {
		fn := discovery.Function{Runtime: "python3.13"}

		got := layerSearchPathEnv(nil, fn, staging)

		want := "PYTHONPATH=/cache/staging/hello/python" + string(os.PathListSeparator) + "/cache/staging/hello/python/lib/python3.13/site-packages"
		if !contains(got, want) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, want)
		}
	})

	t.Run("ruby: RUBYLIB and GEM_PATH (ABI version) are both set", func(t *testing.T) {
		fn := discovery.Function{Runtime: "ruby3.3"}

		got := layerSearchPathEnv(nil, fn, staging)

		wantRubylib := "RUBYLIB=/cache/staging/hello/ruby/lib"
		wantGemPath := "GEM_PATH=/cache/staging/hello/ruby/gems/3.3.0"

		if !contains(got, wantRubylib) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, wantRubylib)
		}
		if !contains(got, wantGemPath) {
			t.Errorf("layerSearchPathEnv() = %v, want it to contain %q", got, wantGemPath)
		}
	})

	t.Run("provided.*: only PATH is set, no language-specific var", func(t *testing.T) {
		fn := discovery.Function{Runtime: "provided.al2023"}

		got := layerSearchPathEnv(nil, fn, staging)

		want := []string{"PATH=/cache/staging/hello/bin"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("layerSearchPathEnv() =\n%v\nwant\n%v", got, want)
		}
	})

	t.Run("existing host PATH is kept first", func(t *testing.T) {
		fn := discovery.Function{Runtime: "provided.al2023"}

		got := layerSearchPathEnv([]string{"PATH=/usr/bin"}, fn, staging)

		want := []string{"PATH=/usr/bin" + string(os.PathListSeparator) + "/cache/staging/hello/bin"}
		if !reflect.DeepEqual(got, want) {
			t.Errorf("layerSearchPathEnv() =\n%v\nwant\n%v", got, want)
		}
	})
}

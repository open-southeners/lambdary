package backend

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func TestDetectContainerCLI(t *testing.T) {
	t.Run("docker present and daemon up wins immediately", func(t *testing.T) {
		r := &fakeRunner{run: map[string]fakeResult{
			"docker": {},
		}}

		cli, err := DetectContainerCLI(context.Background(), r)
		if err != nil {
			t.Fatalf("DetectContainerCLI() unexpected error: %v", err)
		}
		if cli != "docker" {
			t.Errorf("DetectContainerCLI() = %q, want %q", cli, "docker")
		}
		if !reflect.DeepEqual(r.calls, []string{"docker"}) {
			t.Errorf("calls = %v, want only docker probed", r.calls)
		}
	})

	t.Run("probe order: docker down, podman missing, finch up", func(t *testing.T) {
		r := &fakeRunner{run: map[string]fakeResult{
			"docker": {err: errDaemonDown},
			"finch":  {},
		}}

		cli, err := DetectContainerCLI(context.Background(), r)
		if err != nil {
			t.Fatalf("DetectContainerCLI() unexpected error: %v", err)
		}
		if cli != "finch" {
			t.Errorf("DetectContainerCLI() = %q, want %q", cli, "finch")
		}

		want := []string{"docker", "podman", "finch"}
		if !reflect.DeepEqual(r.calls, want) {
			t.Errorf("calls = %v, want %v (probed in order, stopping at first daemon that answers)", r.calls, want)
		}
	})

	t.Run("no CLI binary anywhere", func(t *testing.T) {
		r := &fakeRunner{}

		_, err := DetectContainerCLI(context.Background(), r)
		if err == nil {
			t.Fatal("DetectContainerCLI() expected error, got nil")
		}
		if !errors.Is(err, ErrNoContainerCLI) {
			t.Errorf("DetectContainerCLI() error = %v, want wrapping ErrNoContainerCLI", err)
		}
		if errors.Is(err, ErrDaemonUnreachable) {
			t.Errorf("DetectContainerCLI() error = %v, should not also wrap ErrDaemonUnreachable", err)
		}

		want := []string{"docker", "podman", "finch", "nerdctl"}
		if !reflect.DeepEqual(r.calls, want) {
			t.Errorf("calls = %v, want %v (every candidate probed)", r.calls, want)
		}
	})

	t.Run("CLI present but daemon unreachable", func(t *testing.T) {
		r := &fakeRunner{run: map[string]fakeResult{
			"docker": {err: errDaemonDown},
		}}

		_, err := DetectContainerCLI(context.Background(), r)
		if err == nil {
			t.Fatal("DetectContainerCLI() expected error, got nil")
		}
		if !errors.Is(err, ErrDaemonUnreachable) {
			t.Errorf("DetectContainerCLI() error = %v, want wrapping ErrDaemonUnreachable", err)
		}
		if errors.Is(err, ErrNoContainerCLI) {
			t.Errorf("DetectContainerCLI() error = %v, should not also wrap ErrNoContainerCLI", err)
		}
	})

	t.Run("multiple CLIs present but all daemons unreachable names the first", func(t *testing.T) {
		r := &fakeRunner{run: map[string]fakeResult{
			"docker": {err: errDaemonDown},
			"podman": {err: errDaemonDown},
		}}

		_, err := DetectContainerCLI(context.Background(), r)
		if err == nil {
			t.Fatal("DetectContainerCLI() expected error, got nil")
		}
		if !errors.Is(err, ErrDaemonUnreachable) {
			t.Errorf("DetectContainerCLI() error = %v, want wrapping ErrDaemonUnreachable", err)
		}
	})
}

package process

import (
	"errors"
	"fmt"
	"os/exec"
	"sync"
	"syscall"
	"time"
)

// rieProcess is a handle to a spawned RIE subprocess, driven directly via
// os/exec rather than backend.Runner — see process.go's package doc for
// why. It owns its own process group (SysProcAttr.Setpgid, set at spawn
// time) so stop can signal the RIE and the runtime process it forked
// (node/python3/bootstrap) together.
//
// Signalling by process group (syscall.Kill(-pgid, ...)) and SysProcAttr's
// Setpgid field are POSIX-only; this package targets darwin/linux, matching
// DESIGN.md's non-goal ("Windows support outside WSL" — WSL is Linux).
type rieProcess struct {
	cmd  *exec.Cmd
	out  *outputBuf
	done chan struct{} // closed once cmd.Wait() has returned.

	mu      sync.Mutex
	stopped bool
}

// spawn starts riePath with argv in dir, with env as its full environment
// (already assembled by the caller — see process.go's Start, which appends
// manifestEnv onto an inherited os.Environ()), and its combined
// stdout+stderr captured into an outputBuf. The child is placed in its own
// process group (see rieProcess's doc comment) so stop can tear down the
// whole tree.
func spawn(riePath string, argv []string, dir string, env []string) (*rieProcess, error) {
	cmd := exec.Command(riePath, argv...) //nolint:gosec // riePath and argv are Lambdary-constructed (RIE binary path + resolved runtime command), not arbitrary user input.
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	out := newOutputBuf()
	cmd.Stdout = out
	cmd.Stderr = out

	if err := cmd.Start(); err != nil {
		return nil, err
	}

	p := &rieProcess{cmd: cmd, out: out, done: make(chan struct{})}

	go func() {
		cmd.Wait() //nolint:errcheck // the exit error, if any, isn't actionable here; out.close signals readers regardless of exit status.
		out.close()
		close(p.done)
	}()

	return p, nil
}

// stop terminates the RIE's whole process group: SIGTERM, then SIGKILL if
// it hasn't exited within grace — per plans/m3-process-path.md Unit B ("so
// Stop can kill RIE + its runtime child together"). Safe to call more than
// once; only the first call signals anything.
func (p *rieProcess) stop(grace time.Duration) error {
	p.mu.Lock()
	if p.stopped {
		p.mu.Unlock()
		return nil
	}
	p.stopped = true
	p.mu.Unlock()

	pgid := p.cmd.Process.Pid // the process group leader's pid equals the group id, since Setpgid was set with no explicit Pgid override.

	if err := killGroup(pgid, syscall.SIGTERM); err != nil {
		return err
	}

	select {
	case <-p.done:
		return nil
	case <-time.After(grace):
	}

	if err := killGroup(pgid, syscall.SIGKILL); err != nil {
		return err
	}

	<-p.done

	return nil
}

// killGroup signals every process in pgid's process group, treating "no
// such process" (the group already exited on its own) as success rather
// than an error worth surfacing.
func killGroup(pgid int, sig syscall.Signal) error {
	if err := syscall.Kill(-pgid, sig); err != nil && !errors.Is(err, syscall.ESRCH) {
		return fmt.Errorf("signalling process group %d: %w", pgid, err)
	}

	return nil
}

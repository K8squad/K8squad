/*
Copyright 2026 The K8sAgentMesh Authors.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package mesh

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strconv"
	"sync"
	"sync/atomic"
	"syscall"
	"time"
)

// envSupervisorAddr is the listen address the shim supervisor binds
// (cmd/shim/supervisor.go: KSQUAD_SUPERVISOR_ADDR overrides the :8080 default).
// LocalHost stamps it per instance so N standing processes in one host bind
// distinct ports.
const envSupervisorAddr = "KSQUAD_SUPERVISOR_ADDR"

// LocalHost is the generic POSIX child-process PlatformHost: it execs the shim
// binary as a child process in its own process group (Setpgid), so the adapter's
// SIGSTOP/SIGCONT (Hibernate/Wake) and SIGTERM→SIGKILL (Stop) reach the whole
// group — the runtime CLI and any tool children it spawned — not just the shim
// PID. It is the substrate the prototype gateway (itself PID1 in its pod) runs
// its 2–3 child role-processes on; a Kubernetes sandbox-pod host is the
// k8squad-specific substrate that lands with the warm-pool integration.
//
// LocalHost holds no per-instance state beyond the counter, so one host may
// Launch many instances concurrently.
type LocalHost struct {
	// BinPath is the shim executable launched for each instance (default "shim",
	// resolved on PATH).
	BinPath string
	// Args are the subcommand + args passed to BinPath (default
	// []string{"supervisor"}).
	Args []string
	// Addr assigns the A2A listen addr for the i-th instance (0-based). The
	// default assigns 127.0.0.1:<BasePort+i>. The returned addr is stamped into
	// the child's KSQUAD_SUPERVISOR_ADDR and used to derive Endpoint.
	Addr func(i int) string
	// BasePort is the first port the default Addr assigns (default 8080).
	BasePort int
	// StopGrace is the SIGTERM→SIGKILL window Close waits before escalating
	// (default 5s).
	StopGrace time.Duration
	// Stdout/Stderr receive the child's streams; nil inherits os.Stdout/os.Stderr
	// so the supervisor's run-event stream and diagnostics surface where the host
	// runs. Set them to capture or redirect a child's output.
	Stdout io.Writer
	Stderr io.Writer
	// Sink is the telemetry sink; nil uses NopSink.
	Sink TelemetrySink

	// next is the 0-based instance counter feeding Addr.
	next atomic.Int64
}

// Telemetry returns the host's sink, never nil.
func (h *LocalHost) Telemetry() TelemetrySink {
	if h.Sink != nil {
		return h.Sink
	}
	return NopSink{}
}

// addrFor resolves the listen addr for instance index i.
func (h *LocalHost) addrFor(i int) string {
	if h.Addr != nil {
		return h.Addr(i)
	}
	base := h.BasePort
	if base == 0 {
		base = 8080
	}
	return "127.0.0.1:" + strconv.Itoa(base+i)
}

// Launch execs the shim child in its own process group and returns a running
// Instance. The child inherits the current environment plus spec.Env and the
// per-instance KSQUAD_SUPERVISOR_ADDR; stdout/stderr are inherited so the
// supervisor's run-event stream and diagnostics surface where the host runs.
func (h *LocalHost) Launch(ctx context.Context, spec LaunchSpec) (Instance, error) {
	i := int(h.next.Add(1) - 1)
	addr := h.addrFor(i)

	bin := h.BinPath
	if bin == "" {
		bin = "shim"
	}
	args := h.Args
	if args == nil {
		args = []string{"supervisor"}
	}

	// Not exec.CommandContext: the instance's lifetime is owned by Close (the
	// adapter's Stop), not by the Launch context — tying the process to ctx would
	// kill a standing, successfully-spawned process the moment the spawn call
	// returned.
	// #nosec G204 -- bin/args are the host's own configured shim binary and
	// subcommand (defaults "shim supervisor"), not attacker-controlled input;
	// this mirrors the shim runner's exec discipline (pkg/shim/runner.go).
	cmd := exec.Command(bin, args...)
	cmd.Env = append(os.Environ(), spec.Env...)
	cmd.Env = append(cmd.Env, envSupervisorAddr+"="+addr)
	if spec.Source != nil {
		// Per-process write discipline (ISI-6009, ADR-0024 §1): provision the
		// process's own git worktree off team-main and land it there — never in
		// the shared checkout. A provision failure is a launch failure: falling
		// back into team-main would silently break the isolation this enforces.
		if err := EnsureWorktree(ctx, *spec.Source); err != nil {
			return nil, fmt.Errorf("mesh: launch %s: provision per-process worktree: %w", bin, err)
		}
		cmd.Dir = spec.Source.WorktreeDir
		cmd.Env = append(cmd.Env, spec.Source.Env()...)
	} else if spec.WorkDir != "" {
		cmd.Dir = spec.WorkDir
	}
	if h.Stdout != nil {
		cmd.Stdout = h.Stdout
	} else {
		cmd.Stdout = os.Stdout
	}
	if h.Stderr != nil {
		cmd.Stderr = h.Stderr
	} else {
		cmd.Stderr = os.Stderr
	}
	// Own process group so SIGSTOP/SIGCONT/SIGTERM reach the runtime CLI and its
	// tool children, mirroring the shim runner's Setpgid discipline
	// (pkg/shim/runner.go) — a group signal, never a lone-PID signal.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("mesh: start %s %v: %w", bin, args, err)
	}

	inst := &localInstance{
		id:        fmt.Sprintf("%s-%d-%d", spec.RuntimeType, i, cmd.Process.Pid),
		endpoint:  "http://" + addr,
		cmd:       cmd,
		stopGrace: h.StopGrace,
		done:      make(chan struct{}),
	}
	go inst.reap()
	return inst, nil
}

// localInstance is one shim child process LocalHost manages.
type localInstance struct {
	id        string
	endpoint  string
	cmd       *exec.Cmd
	stopGrace time.Duration

	// done closes when the process has been reaped (Wait returned).
	done      chan struct{}
	closeOnce sync.Once
}

func (in *localInstance) ID() string       { return in.id }
func (in *localInstance) Endpoint() string { return in.endpoint }

// reap waits on the process so the kernel does not leave a zombie, then signals
// done. It runs for the instance's whole life.
func (in *localInstance) reap() {
	_ = in.cmd.Wait()
	close(in.done)
}

// pgid is the negated PID — the process-group id a group signal targets.
func (in *localInstance) pgid() int { return -in.cmd.Process.Pid }

// Signal delivers sig to the instance's whole process group. SIGSTOP/SIGCONT
// (Hibernate/Wake) and any other signal the adapter needs ride this path. It is
// a no-op error ("process already finished") once the child has exited, which
// the caller can treat as success for idempotent Wake.
func (in *localInstance) Signal(sig syscall.Signal) error {
	if in.cmd.Process == nil {
		return fmt.Errorf("mesh: instance %s has no process", in.id)
	}
	select {
	case <-in.done:
		return fmt.Errorf("mesh: instance %s already exited", in.id)
	default:
	}
	if err := syscall.Kill(in.pgid(), sig); err != nil {
		return fmt.Errorf("mesh: signal %v to %s: %w", sig, in.id, err)
	}
	return nil
}

// Close terminates the process group with SIGTERM, waits up to StopGrace for a
// clean exit, then escalates to SIGKILL. A process that is SIGSTOP-frozen is
// SIGCONT'd first so it can observe the SIGTERM (a stopped process cannot run
// its signal handler). Idempotent: a second Close is a no-op once reaped.
func (in *localInstance) Close() error {
	if in.cmd.Process == nil {
		return nil
	}
	in.closeOnce.Do(func() {
		pgid := in.pgid()
		// Unfreeze first so a hibernated process can act on the SIGTERM below.
		_ = syscall.Kill(pgid, syscall.SIGCONT)
		_ = syscall.Kill(pgid, syscall.SIGTERM)
		grace := in.stopGrace
		if grace <= 0 {
			grace = 5 * time.Second
		}
		select {
		case <-in.done:
		case <-time.After(grace):
			_ = syscall.Kill(pgid, syscall.SIGKILL)
			<-in.done
		}
	})
	return nil
}

var (
	_ PlatformHost = (*LocalHost)(nil)
	_ Instance     = (*localInstance)(nil)
)

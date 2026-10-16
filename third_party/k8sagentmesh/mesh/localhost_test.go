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
	"io"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// launchSleeper starts a long-lived child (`sleep`) under LocalHost, discarding
// its streams so no pipe is held open, and registers a cleanup Close so the
// child never outlives the test (a leaked, running child would hold the test's
// fds open and wedge the run). It returns the instance and the child's PID.
func launchSleeper(t *testing.T) (Instance, int) {
	t.Helper()
	if _, err := exec.LookPath("sleep"); err != nil {
		t.Skip("sleep not available")
	}
	host := &LocalHost{
		BinPath:   "sleep",
		Args:      []string{"3600"},
		StopGrace: 500 * time.Millisecond,
		Stdout:    io.Discard,
		Stderr:    io.Discard,
		Addr:      func(int) string { return "127.0.0.1:0" },
	}
	inst, err := host.Launch(context.Background(), LaunchSpec{RuntimeType: "test"})
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}
	t.Cleanup(func() { _ = inst.Close() })
	return inst, pidFromID(t, inst.ID())
}

// pidFromID recovers the PID from a LocalHost instance id ("<rt>-<i>-<pid>").
func pidFromID(t *testing.T, id string) int {
	t.Helper()
	i := strings.LastIndex(id, "-")
	if i < 0 {
		t.Fatalf("instance id %q has no pid suffix", id)
	}
	pid, err := strconv.Atoi(id[i+1:])
	if err != nil {
		t.Fatalf("instance id %q: bad pid: %v", id, err)
	}
	return pid
}

// procState reads the single-char process state from /proc/<pid>/stat (field 3,
// after the parenthesized comm). 'T' is stopped (SIGSTOP), 'R'/'S'/'D' are
// runnable/sleeping. Returns 0 if the process is gone.
func procState(pid int) byte {
	b, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return 0
	}
	// comm may contain spaces/parens; the state char is the first token after
	// the closing ')'.
	s := string(b)
	rparen := strings.LastIndexByte(s, ')')
	if rparen < 0 || rparen+2 >= len(s) {
		return 0
	}
	return s[rparen+2]
}

// waitState polls until the process reaches want, or fails after a timeout.
func waitState(t *testing.T, pid int, want byte, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if procState(pid) == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("process %d did not reach state %q within %s (last=%q)", pid, string(want), timeout, string(procState(pid)))
}

func TestLocalHostLaunchAndStop(t *testing.T) {
	inst, pid := launchSleeper(t)

	if !strings.HasPrefix(inst.ID(), "test-0-") {
		t.Fatalf("instance id = %q, want test-0-<pid>", inst.ID())
	}
	if inst.Endpoint() != "http://127.0.0.1:0" {
		t.Fatalf("endpoint = %q, want http://127.0.0.1:0", inst.Endpoint())
	}
	if st := procState(pid); st == 0 {
		t.Fatal("child is not running right after Launch")
	}

	// Close terminates the whole group; the reaper must observe the exit.
	done := make(chan struct{})
	go func() { _ = inst.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not return within grace")
	}
	waitState(t, pid, 0, 2*time.Second) // process is gone

	// A signal after exit is reported, not a panic.
	if err := inst.Signal(syscall.SIGCONT); err == nil {
		t.Fatal("Signal to an exited instance should error")
	}
	// Close is idempotent.
	if err := inst.Close(); err != nil {
		t.Fatalf("second Close: %v", err)
	}
}

// TestLocalHostHibernateWakeFreezesProcess is the load-bearing evidence for the
// consult gate (ISI-5809 §11.1 b/d): SIGSTOP/SIGCONT really do freeze and resume
// a child-process runtime, so they are a viable base Hibernate/Wake. It asserts
// the kernel-reported process state, not a side effect.
func TestLocalHostHibernateWakeFreezesProcess(t *testing.T) {
	inst, pid := launchSleeper(t)

	// Hibernate (SIGSTOP): the kernel marks the process stopped ('T').
	if err := inst.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	waitState(t, pid, 'T', 2*time.Second)

	// Wake (SIGCONT): the process leaves the stopped state.
	if err := inst.Signal(syscall.SIGCONT); err != nil {
		t.Fatalf("SIGCONT: %v", err)
	}
	waitState(t, pid, 'S', 2*time.Second)
}

// TestLocalHostCloseUnfreezesBeforeTerminate proves Close can tear down a
// hibernated process: a SIGSTOP'd process cannot act on SIGTERM, so Close
// SIGCONTs it first (then SIGTERM, then SIGKILL on grace).
func TestLocalHostCloseUnfreezesBeforeTerminate(t *testing.T) {
	inst, pid := launchSleeper(t)

	if err := inst.Signal(syscall.SIGSTOP); err != nil {
		t.Fatalf("SIGSTOP: %v", err)
	}
	waitState(t, pid, 'T', 2*time.Second)

	done := make(chan struct{})
	go func() { _ = inst.Close(); close(done) }()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("Close did not tear down a hibernated process within grace")
	}
	waitState(t, pid, 0, 2*time.Second)
}

func TestLocalHostAddrAndTelemetryDefaults(t *testing.T) {
	h := &LocalHost{}
	if h.Telemetry() == nil {
		t.Fatal("Telemetry must never be nil")
	}
	if got := h.addrFor(0); got != "127.0.0.1:8080" {
		t.Fatalf("addrFor(0) = %q, want 127.0.0.1:8080", got)
	}
	if got := h.addrFor(2); got != "127.0.0.1:8082" {
		t.Fatalf("addrFor(2) = %q, want 127.0.0.1:8082", got)
	}
	h2 := &LocalHost{BasePort: 9000}
	if got := h2.addrFor(1); got != "127.0.0.1:9001" {
		t.Fatalf("addrFor(1) with BasePort 9000 = %q, want 127.0.0.1:9001", got)
	}
}

func TestLocalHostLaunchBadBinary(t *testing.T) {
	h := &LocalHost{BinPath: "/nonexistent/shim-xyz", Args: []string{"supervisor"}}
	if _, err := h.Launch(context.Background(), LaunchSpec{RuntimeType: "x"}); err == nil {
		t.Fatal("Launch should fail for a missing binary")
	}
}

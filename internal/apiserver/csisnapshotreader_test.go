package apiserver

// ADR-0025 D4 unit tests for CSISnapshotBusyReader (ISI-5350).
//
// Coverage:
//   - Non-CSI cluster: all three methods return ErrNoWorkspaceSnapshot.
//   - CSI available + spec error: propagates error (no kube needed — spec fails before kube).
//   - hasCSI caches the probe result (discovery called only once).
//   - hasCSI correctly reports group presence / absence.
//   - Compile-time: *CSISnapshotBusyReader implements BusySnapshotReader.

import (
	"context"
	"errors"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
)

// ---- fakes ----------------------------------------------------------------

type fakeSnapshotSpecProvider struct {
	ns, pvcName, saName string
	err                 error
}

func (f *fakeSnapshotSpecProvider) SnapshotSpec(_ context.Context, _ string) (string, string, string, error) {
	return f.ns, f.pvcName, f.saName, f.err
}

type fakeGroupLister struct {
	groups []string
	calls  int
}

func (f *fakeGroupLister) ServerGroups() (*metav1.APIGroupList, error) {
	f.calls++
	groups := make([]metav1.APIGroup, 0, len(f.groups))
	for _, g := range f.groups {
		groups = append(groups, metav1.APIGroup{Name: g})
	}
	return &metav1.APIGroupList{Groups: groups}, nil
}

// ---- tests ----------------------------------------------------------------

// TestCSISnapshotBusyReader_NonCSI verifies that on clusters without VolumeSnapshot CRD,
// all three methods return ErrNoWorkspaceSnapshot immediately (ADR-0025 D4 infra-gate).
// No kube client is needed because the probe returns false before any kube call.
func TestCSISnapshotBusyReader_NonCSI(t *testing.T) {
	disc := &fakeGroupLister{groups: []string{"apps", "batch"}} // no snapshot.storage.k8s.io
	specs := &fakeSnapshotSpecProvider{ns: "team-ns", pvcName: "workspace-project-p1", saName: "agent"}
	r := NewCSISnapshotBusyReader(disc, nil, readerpod.DisabledLauncher{}, readerpod.Config{}, specs)

	ctx := context.Background()
	if _, err := r.ListSnapshotDir(ctx, "proj-1", ".", 0); !errors.Is(err, ErrNoWorkspaceSnapshot) {
		t.Errorf("ListSnapshotDir on non-CSI: want ErrNoWorkspaceSnapshot, got %v", err)
	}
	if _, err := r.ReadSnapshotFile(ctx, "proj-1", "foo.txt", 0, 0); !errors.Is(err, ErrNoWorkspaceSnapshot) {
		t.Errorf("ReadSnapshotFile on non-CSI: want ErrNoWorkspaceSnapshot, got %v", err)
	}
	if _, err := r.StatSnapshotFile(ctx, "proj-1", "foo.txt"); !errors.Is(err, ErrNoWorkspaceSnapshot) {
		t.Errorf("StatSnapshotFile on non-CSI: want ErrNoWorkspaceSnapshot, got %v", err)
	}
}

// TestCSISnapshotBusyReader_ProbeCache verifies the discovery client is called only once
// across multiple method calls (result must be cached, not re-probed per request).
func TestCSISnapshotBusyReader_ProbeCache(t *testing.T) {
	disc := &fakeGroupLister{groups: []string{}} // non-CSI — returns early before any kube call
	r := NewCSISnapshotBusyReader(disc, nil, readerpod.DisabledLauncher{}, readerpod.Config{}, nil)

	ctx := context.Background()
	_, _ = r.ListSnapshotDir(ctx, "p", ".", 0)
	_, _ = r.ReadSnapshotFile(ctx, "p", "f", 0, 0)
	_, _ = r.StatSnapshotFile(ctx, "p", "f")

	if disc.calls != 1 {
		t.Errorf("discovery probed %d times, want 1 (result must be cached)", disc.calls)
	}
}

// TestCSISnapshotBusyReader_HasCSI_Present verifies hasCSI returns true when the group is listed.
func TestCSISnapshotBusyReader_HasCSI_Present(t *testing.T) {
	disc := &fakeGroupLister{groups: []string{volumeSnapshotGroup}}
	r := NewCSISnapshotBusyReader(disc, nil, nil, readerpod.Config{}, nil)
	if !r.hasCSI() {
		t.Error("hasCSI: want true when VolumeSnapshot group present")
	}
}

// TestCSISnapshotBusyReader_HasCSI_Absent verifies hasCSI returns false when absent.
func TestCSISnapshotBusyReader_HasCSI_Absent(t *testing.T) {
	disc := &fakeGroupLister{groups: []string{"apps", "batch"}}
	r := NewCSISnapshotBusyReader(disc, nil, nil, readerpod.Config{}, nil)
	if r.hasCSI() {
		t.Error("hasCSI: want false when VolumeSnapshot group absent")
	}
}

// TestCSISnapshotBusyReader_SpecError verifies spec resolution errors are propagated.
// The probe returns CSI available, but the spec fails before any kube call — so no kube needed.
func TestCSISnapshotBusyReader_SpecError(t *testing.T) {
	disc := &fakeGroupLister{groups: []string{volumeSnapshotGroup}}
	specs := &fakeSnapshotSpecProvider{err: errors.New("project not found")}
	r := NewCSISnapshotBusyReader(disc, nil, readerpod.DisabledLauncher{}, readerpod.Config{}, specs)

	ctx := context.Background()
	_, err := r.ListSnapshotDir(ctx, "bad-project", ".", 0)
	if err == nil {
		t.Fatal("spec error: want non-nil error, got nil")
	}
	if errors.Is(err, ErrNoWorkspaceSnapshot) {
		t.Errorf("spec error should not be ErrNoWorkspaceSnapshot — it must propagate the real error")
	}
}

// Compile-time assertion: *CSISnapshotBusyReader implements BusySnapshotReader.
var _ BusySnapshotReader = (*CSISnapshotBusyReader)(nil)

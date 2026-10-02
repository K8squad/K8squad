package apiserver

// csisnapshotreader.go — ADR-0025 D4: CSI point-in-time snapshot read for busy / non-git workspaces.
//
// When a running agent holds the project workspace PVC (RWO), a reader pod cannot co-mount it.
// This file implements BusySnapshotReader (files.go) by:
//  1. Probing for VolumeSnapshot CRD availability (cached after first probe).
//  2. On CSI-capable clusters: creating a VolumeSnapshot of the project PVC, waiting for
//     ReadyToUse, creating a PVC-from-snapshot, launching a reader pod against it, delegating
//     List/Read/Stat, then cleaning up snapshot + PVC + pod.
//  3. On non-CSI clusters: returning ErrNoWorkspaceSnapshot so the route renders the honest
//     "snapshot unavailable" degraded state — never a fabricated empty listing.
//
// The snapshot PVC is read-only (no RWO contention; solves F9 from ADR-0025 context) and is
// always cleaned up: TTL-bounded context on the pod, defer-cleanup in each operation.
//
// Infra-gate: the test cluster runs local-path (no VolumeSnapshot support). The probe returns
// false there, so all three methods immediately return ErrNoWorkspaceSnapshot — the honest floor.

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod"
	"github.com/K8squad/K8squad/internal/buildbrowser/readerpod/readclient"
)

// groupLister is the narrow discovery surface the CSI capability probe uses. The full
// discovery.DiscoveryInterface implements it; tests supply a minimal fake.
type groupLister interface {
	ServerGroups() (*metav1.APIGroupList, error)
}

// volumeSnapshotGroup is the API group for the CSI VolumeSnapshot CRD installed by
// external-snapshotter (https://github.com/kubernetes-csi/external-snapshotter).
const volumeSnapshotGroup = "snapshot.storage.k8s.io"

// csiSnapshotTimeout is the per-operation deadline: snapshot-readiness wait + pod-readiness wait.
// Must stay well inside the 45 s readerTimeout (files.go) so the outer context cancels us cleanly.
const csiSnapshotTimeout = 40 * time.Second

// snapshotReadyPollInterval is how often we poll the VolumeSnapshot for ReadyToUse.
const snapshotReadyPollInterval = 2 * time.Second

// snapshotPVCLabel labels VolumeSnapshots and PVCs created by this reader so they can be found
// and cleaned up on orphan sweeps (e.g. after an apiserver restart).
const snapshotPVCLabel = "k8squad.io/snapshot-reader"

// SnapshotSpecProvider resolves the namespace, PVC name and reader ServiceAccount for a project.
// The CSI snapshot reader needs these independently of the live-read spec resolver (which also does
// a busy check and a browse-target lookup that are irrelevant on the snapshot path).
type SnapshotSpecProvider interface {
	// SnapshotSpec returns (namespace, pvcName, readerSAName) for the given projectID, or an error.
	// It never blocks on a coord busy check; it only resolves the PVC address and namespace.
	SnapshotSpec(ctx context.Context, projectID string) (namespace, pvcName, readerSAName string, err error)
}

// CSISnapshotBusyReader is the ADR-0025 D4 BusySnapshotReader.
type CSISnapshotBusyReader struct {
	discovery groupLister
	kube      client.Client
	launcher  readerpod.Launcher
	cfg       readerpod.Config
	specs     SnapshotSpecProvider
	dial      func(baseURL string) readClient

	// csiProbed / csiAvailable are the lazy capability-probe cache.
	probeMu      sync.Mutex
	csiProbed    bool
	csiAvailable bool
}

// NewCSISnapshotBusyReader builds a CSI-backed BusySnapshotReader.
//
//   - disc: discovery client for the capability probe (can be built from the same rest.Config as kube).
//   - kube: direct (uncached) client for creating/deleting VolumeSnapshot + PVC objects.
//   - launcher: reader-pod launcher (same one as the live-read path).
//   - cfg: reader-pod config (same as the live-read path).
//   - specs: project-to-namespace/PVC resolver.
func NewCSISnapshotBusyReader(
	disc groupLister,
	kube client.Client,
	launcher readerpod.Launcher,
	cfg readerpod.Config,
	specs SnapshotSpecProvider,
) *CSISnapshotBusyReader {
	return &CSISnapshotBusyReader{
		discovery: disc,
		kube:      kube,
		launcher:  launcher,
		cfg:       cfg,
		specs:     specs,
		dial:      func(baseURL string) readClient { return readclient.New(baseURL, nil) },
	}
}

// ListSnapshotDir implements BusySnapshotReader.
func (r *CSISnapshotBusyReader) ListSnapshotDir(ctx context.Context, projectID, dirPath string, page int) (*DirListing, error) {
	rc, cleanup, err := r.snapshotReader(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	wire, err := rc.List(ctx, dirPath, page)
	if err != nil {
		return nil, mapReadErr(err)
	}
	entries := make([]DirEntry, 0, len(wire.Entries))
	for _, e := range wire.Entries {
		entries = append(entries, DirEntry{Name: e.Name, Type: e.Type, Size: e.Size})
	}
	return &DirListing{Entries: entries, NextPage: wire.NextPage}, nil
}

// ReadSnapshotFile implements BusySnapshotReader.
func (r *CSISnapshotBusyReader) ReadSnapshotFile(ctx context.Context, projectID, filePath string, offset, length int64) (*FileContent, error) {
	rc, cleanup, err := r.snapshotReader(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	wire, err := rc.Read(ctx, filePath, offset, length)
	if err != nil {
		return nil, mapReadErr(err)
	}
	return &FileContent{
		Data:        wire.Data,
		Size:        wire.Size,
		ContentType: wire.ContentType,
		Offset:      wire.Offset,
		Length:      wire.Length,
	}, nil
}

// StatSnapshotFile implements BusySnapshotReader.
func (r *CSISnapshotBusyReader) StatSnapshotFile(ctx context.Context, projectID, filePath string) (*FileStat, error) {
	rc, cleanup, err := r.snapshotReader(ctx, projectID)
	if err != nil {
		return nil, err
	}
	defer cleanup()

	wire, err := rc.Stat(ctx, filePath)
	if err != nil {
		return nil, mapReadErr(err)
	}
	st := &FileStat{
		Name:    wire.Name,
		Type:    wire.Type,
		Size:    wire.Size,
		ModTime: wire.ModTime,
	}
	if wire.Git != nil {
		st.Git = &GitChange{
			CommitHash: wire.Git.CommitHash,
			Author:     wire.Git.Author,
			Message:    wire.Git.Message,
			Timestamp:  wire.Git.Timestamp,
		}
	}
	return st, nil
}

// snapshotReader probes CSI, creates a VolumeSnapshot + PVC, launches a reader pod, and
// returns a live readClient bound to it plus a cleanup func the caller MUST defer.
// Returns ErrNoWorkspaceSnapshot when CSI is unavailable (non-CSI cluster).
func (r *CSISnapshotBusyReader) snapshotReader(ctx context.Context, projectID string) (readClient, func(), error) {
	if !r.hasCSI() {
		return nil, nil, ErrNoWorkspaceSnapshot
	}

	ns, pvcName, saName, err := r.specs.SnapshotSpec(ctx, projectID)
	if err != nil {
		return nil, nil, fmt.Errorf("csisnapshotreader: resolve spec for %s: %w", projectID, err)
	}

	opCtx, cancel := context.WithTimeout(ctx, csiSnapshotTimeout)
	defer cancel()

	// Create VolumeSnapshot.
	snapName := fmt.Sprintf("reader-snap-%s", projectID[:8])
	if err := r.createVolumeSnapshot(opCtx, snapName, ns, pvcName); err != nil {
		return nil, noop, fmt.Errorf("csisnapshotreader: create snapshot for %s: %w", projectID, err)
	}

	// Wait for ReadyToUse.
	if err := r.waitSnapshotReady(opCtx, snapName, ns); err != nil {
		_ = r.deleteVolumeSnapshot(context.Background(), snapName, ns)
		return nil, noop, fmt.Errorf("csisnapshotreader: snapshot not ready for %s: %w", projectID, err)
	}

	// Create PVC from snapshot.
	snapPVCName := fmt.Sprintf("reader-snappvc-%s", projectID[:8])
	if err := r.createSnapshotPVC(opCtx, snapPVCName, snapName, ns); err != nil {
		_ = r.deleteVolumeSnapshot(context.Background(), snapName, ns)
		return nil, noop, fmt.Errorf("csisnapshotreader: create snapshot pvc for %s: %w", projectID, err)
	}

	// Launch reader pod against the snapshot PVC.
	spec := readerpod.Spec{
		RunID:          fmt.Sprintf("snap-%s", projectID[:8]),
		ProjectPVCName: snapPVCName,
		ReaderSAName:   saName,
		Namespace:      ns,
	}
	handle, err := r.launcher.Launch(opCtx, spec)
	if err != nil {
		_ = r.deleteSnapshotPVC(context.Background(), snapPVCName, ns)
		_ = r.deleteVolumeSnapshot(context.Background(), snapName, ns)
		if errors.Is(err, readerpod.ErrDisabled) {
			return nil, noop, ErrNoWorkspaceSnapshot
		}
		return nil, noop, fmt.Errorf("csisnapshotreader: launch reader for %s: %w", projectID, err)
	}

	base := handle.BaseURL()
	if base == "" {
		_ = r.launcher.TearDown(context.Background(), handle)
		_ = r.deleteSnapshotPVC(context.Background(), snapPVCName, ns)
		_ = r.deleteVolumeSnapshot(context.Background(), snapName, ns)
		return nil, noop, ErrNoWorkspaceSnapshot
	}
	if err := waitReaderReady(opCtx, base); err != nil {
		_ = r.launcher.TearDown(context.Background(), handle)
		_ = r.deleteSnapshotPVC(context.Background(), snapPVCName, ns)
		_ = r.deleteVolumeSnapshot(context.Background(), snapName, ns)
		return nil, noop, fmt.Errorf("csisnapshotreader: reader not ready for %s: %w", projectID, err)
	}

	cleanup := func() {
		bg := context.Background()
		_ = r.launcher.TearDown(bg, handle)
		_ = r.deleteSnapshotPVC(bg, snapPVCName, ns)
		_ = r.deleteVolumeSnapshot(bg, snapName, ns)
	}
	return r.dial(base), cleanup, nil
}

func noop() {}

// hasCSI returns true if the cluster has VolumeSnapshot CRD support. The result is cached after
// the first probe so repeated requests (every busy read) don't hammer the API server.
func (r *CSISnapshotBusyReader) hasCSI() bool {
	r.probeMu.Lock()
	defer r.probeMu.Unlock()
	if r.csiProbed {
		return r.csiAvailable
	}
	groups, err := r.discovery.ServerGroups()
	if err == nil {
		for _, g := range groups.Groups {
			if g.Name == volumeSnapshotGroup {
				r.csiAvailable = true
				break
			}
		}
	}
	r.csiProbed = true
	return r.csiAvailable
}

func (r *CSISnapshotBusyReader) createVolumeSnapshot(ctx context.Context, name, ns, sourcePVC string) error {
	snap := &unstructured.Unstructured{
		Object: map[string]interface{}{
			"apiVersion": volumeSnapshotGroup + "/v1",
			"kind":       "VolumeSnapshot",
			"metadata": map[string]interface{}{
				"name":      name,
				"namespace": ns,
				"labels":    map[string]interface{}{snapshotPVCLabel: "true"},
			},
			"spec": map[string]interface{}{
				"source": map[string]interface{}{
					"persistentVolumeClaimName": sourcePVC,
				},
			},
		},
	}
	return r.kube.Create(ctx, snap)
}

func (r *CSISnapshotBusyReader) waitSnapshotReady(ctx context.Context, name, ns string) error {
	for {
		snap := &unstructured.Unstructured{}
		snap.SetGroupVersionKind(schema.GroupVersionKind{
			Group:   volumeSnapshotGroup,
			Version: "v1",
			Kind:    "VolumeSnapshot",
		})
		if err := r.kube.Get(ctx, client.ObjectKey{Name: name, Namespace: ns}, snap); err != nil {
			return fmt.Errorf("get snapshot: %w", err)
		}
		ready, _, _ := unstructured.NestedBool(snap.Object, "status", "readyToUse")
		if ready {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(snapshotReadyPollInterval):
		}
	}
}

func (r *CSISnapshotBusyReader) deleteVolumeSnapshot(ctx context.Context, name, ns string) error {
	snap := &unstructured.Unstructured{}
	snap.SetGroupVersionKind(schema.GroupVersionKind{
		Group: volumeSnapshotGroup, Version: "v1", Kind: "VolumeSnapshot",
	})
	snap.SetName(name)
	snap.SetNamespace(ns)
	return client.IgnoreNotFound(r.kube.Delete(ctx, snap))
}

func (r *CSISnapshotBusyReader) createSnapshotPVC(ctx context.Context, name, snapName, ns string) error {
	apiGroup := volumeSnapshotGroup
	pvc := &corev1.PersistentVolumeClaim{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: ns,
			Labels:    map[string]string{snapshotPVCLabel: "true"},
		},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadOnlyMany},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{
					corev1.ResourceStorage: resource.MustParse("1Mi"), // Ignored for snapshot-sourced PVCs; required by API.
				},
			},
			DataSource: &corev1.TypedLocalObjectReference{
				APIGroup: &apiGroup,
				Kind:     "VolumeSnapshot",
				Name:     snapName,
			},
		},
	}
	return r.kube.Create(ctx, pvc)
}

func (r *CSISnapshotBusyReader) deleteSnapshotPVC(ctx context.Context, name, ns string) error {
	pvc := &corev1.PersistentVolumeClaim{}
	pvc.Name = name
	pvc.Namespace = ns
	return client.IgnoreNotFound(r.kube.Delete(ctx, pvc))
}

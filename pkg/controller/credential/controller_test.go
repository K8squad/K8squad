/*
Copyright 2026 The K8squad Authors.

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

package credential

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

var fixedNow = time.Date(2026, 8, 24, 12, 0, 0, 0, time.UTC)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := corev1.AddToScheme(s); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	return s
}

// humanSeatSecret builds a human-seat OAuth Secret expiring at expiresAt.
func humanSeatSecret(expiresAt time.Time) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "claude-oauth",
			Namespace: "squad-a",
			Labels:    map[string]string{LabelCredentialClass: ClassHumanSeat},
		},
		Data: map[string][]byte{
			KeyAccessToken:  []byte("access-old"),
			KeyRefreshToken: []byte("refresh-old"),
			KeyExpiresAt:    []byte(expiresAt.UTC().Format(time.RFC3339)),
			KeyConnectedAt:  []byte(fixedNow.Add(-24 * time.Hour).UTC().Format(time.RFC3339)),
		},
	}
}

func newReconciler(t *testing.T, refresher TokenRefresher, objs ...client.Object) (*Reconciler, client.Client) {
	t.Helper()
	c := fake.NewClientBuilder().WithScheme(testScheme(t)).WithObjects(objs...).Build()
	return &Reconciler{
		Client:    c,
		Refresher: refresher,
		Now:       func() time.Time { return fixedNow },
	}, c
}

func reconcile(t *testing.T, r *Reconciler, ns, name string) ctrl.Result {
	t.Helper()
	res, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: ns, Name: name}})
	if err != nil {
		t.Fatalf("reconcile error: %v", err)
	}
	return res
}

func getSecret(t *testing.T, c client.Client, ns, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &s); err != nil {
		t.Fatalf("get secret: %v", err)
	}
	return &s
}

// Token far from expiry: no refresh, connected, requeue near the refresh instant.
func TestReconcile_NotYetDue_StaysConnected(t *testing.T) {
	f := &fakeRefresher{}
	sec := humanSeatSecret(fixedNow.Add(4 * time.Hour))
	r, c := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	if f.calls != 0 {
		t.Errorf("refresher called %d times, want 0 (not yet in lead window)", f.calls)
	}
	got := getSecret(t, c, "squad-a", "claude-oauth")
	if got.Annotations[AnnotationState] != StateConnected {
		t.Errorf("state = %q, want connected", got.Annotations[AnnotationState])
	}
	if string(got.Data[KeyAccessToken]) != "access-old" {
		t.Errorf("access token was modified when no refresh was due")
	}
	// requeue ~ (expiry - lead) - now = 4h - 30m = 3h30m
	want := 4*time.Hour - DefaultRefreshLead
	if res.RequeueAfter < want-time.Minute || res.RequeueAfter > want+time.Minute {
		t.Errorf("RequeueAfter = %v, want ~%v", res.RequeueAfter, want)
	}
}

// Inside the lead window: refresh, write new material to the SAME Secret.
func TestReconcile_DueForRefresh_WritesBackInPlace(t *testing.T) {
	newExpiry := fixedNow.Add(8 * time.Hour)
	f := &fakeRefresher{result: RefreshedToken{AccessToken: "access-new", RefreshToken: "refresh-new", ExpiresAt: newExpiry}}
	sec := humanSeatSecret(fixedNow.Add(10 * time.Minute)) // within 30m lead
	r, c := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	if f.calls != 1 {
		t.Fatalf("refresher called %d times, want 1", f.calls)
	}
	got := getSecret(t, c, "squad-a", "claude-oauth")
	if string(got.Data[KeyAccessToken]) != "access-new" {
		t.Errorf("access token = %q, want access-new (in-place rewrite)", got.Data[KeyAccessToken])
	}
	if string(got.Data[KeyRefreshToken]) != "refresh-new" {
		t.Errorf("refresh token = %q, want refresh-new (rotation persisted)", got.Data[KeyRefreshToken])
	}
	if string(got.Data[KeyExpiresAt]) != newExpiry.UTC().Format(time.RFC3339) {
		t.Errorf("expiresAt = %q, want %v", got.Data[KeyExpiresAt], newExpiry)
	}
	if got.Annotations[AnnotationState] != StateConnected {
		t.Errorf("state = %q, want connected after refresh", got.Annotations[AnnotationState])
	}
	if got.Annotations[AnnotationLastRefresh] != fixedNow.UTC().Format(time.RFC3339) {
		t.Errorf("last-refresh annotation = %q, want %v (canary heartbeat)", got.Annotations[AnnotationLastRefresh], fixedNow)
	}
	// name (Secret identity) is unchanged — concurrent pods keep the same mount.
	if got.Name != "claude-oauth" {
		t.Errorf("Secret name changed to %q", got.Name)
	}
	want := 8*time.Hour - DefaultRefreshLead
	if res.RequeueAfter < want-time.Minute || res.RequeueAfter > want+time.Minute {
		t.Errorf("RequeueAfter = %v, want ~%v", res.RequeueAfter, want)
	}
}

// Refresh token rejected → terminal expired, no requeue, token left intact.
func TestReconcile_InvalidGrant_MarksExpired(t *testing.T) {
	f := &fakeRefresher{err: ErrRefreshExpired}
	sec := humanSeatSecret(fixedNow.Add(5 * time.Minute))
	r, c := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	got := getSecret(t, c, "squad-a", "claude-oauth")
	if got.Annotations[AnnotationState] != StateExpired {
		t.Errorf("state = %q, want expired", got.Annotations[AnnotationState])
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (expired needs human re-login, not auto-retry)", res.RequeueAfter)
	}
	if string(got.Data[KeyAccessToken]) != "access-old" {
		t.Errorf("expired path must not clobber the last-known token")
	}
}

// Transient refresh failure → stays connected (token still valid), short requeue.
func TestReconcile_TransientError_RetriesSoon(t *testing.T) {
	f := &fakeRefresher{err: context.DeadlineExceeded}
	sec := humanSeatSecret(fixedNow.Add(20 * time.Minute))
	r, c := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	got := getSecret(t, c, "squad-a", "claude-oauth")
	if got.Annotations[AnnotationState] != StateConnected {
		t.Errorf("state = %q, want connected (transient failure keeps the still-valid token)", got.Annotations[AnnotationState])
	}
	if res.RequeueAfter != DefaultErrorRequeue {
		t.Errorf("RequeueAfter = %v, want %v", res.RequeueAfter, DefaultErrorRequeue)
	}
}

// Human-seat Secret missing its refresh token → StateError, no refresh attempt.
func TestReconcile_MissingRefreshToken_MarksError(t *testing.T) {
	f := &fakeRefresher{}
	sec := humanSeatSecret(fixedNow.Add(1 * time.Hour))
	delete(sec.Data, KeyRefreshToken)
	r, c := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	if f.calls != 0 {
		t.Errorf("refresher called on a non-refreshable Secret")
	}
	got := getSecret(t, c, "squad-a", "claude-oauth")
	if got.Annotations[AnnotationState] != StateError {
		t.Errorf("state = %q, want error", got.Annotations[AnnotationState])
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0 (fixed by correcting the Secret)", res.RequeueAfter)
	}
}

// A service-account Secret that somehow reaches Reconcile is ignored (defence in
// depth behind the Watch predicate) — ADR-041 enforcement.
func TestReconcile_ServiceAccountSecret_Ignored(t *testing.T) {
	f := &fakeRefresher{}
	sec := humanSeatSecret(fixedNow.Add(1 * time.Minute))
	sec.Labels[LabelCredentialClass] = "service-account"
	r, _ := newReconciler(t, f, sec)

	res := reconcile(t, r, "squad-a", "claude-oauth")

	if f.calls != 0 {
		t.Errorf("refresher must never run against a service-account credential")
	}
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
	}
}

// Already-expired-but-refreshable token (past expiry, refresh token still good)
// refreshes immediately rather than stranding.
func TestReconcile_PastExpiry_RefreshesImmediately(t *testing.T) {
	f := &fakeRefresher{result: RefreshedToken{AccessToken: "a", RefreshToken: "r", ExpiresAt: fixedNow.Add(8 * time.Hour)}}
	sec := humanSeatSecret(fixedNow.Add(-2 * time.Hour)) // already expired
	r, c := newReconciler(t, f, sec)

	reconcile(t, r, "squad-a", "claude-oauth")

	if f.calls != 1 {
		t.Fatalf("refresher called %d times, want 1", f.calls)
	}
	got := getSecret(t, c, "squad-a", "claude-oauth")
	if got.Annotations[AnnotationState] != StateConnected {
		t.Errorf("state = %q, want connected", got.Annotations[AnnotationState])
	}
}

func TestReconcile_SecretDeleted_NoError(t *testing.T) {
	f := &fakeRefresher{}
	r, _ := newReconciler(t, f) // no objects
	res := reconcile(t, r, "squad-a", "gone")
	if res.RequeueAfter != 0 {
		t.Errorf("RequeueAfter = %v, want 0", res.RequeueAfter)
	}
}

func TestSetupWithManager_RequiresRefresher(t *testing.T) {
	r := &Reconciler{}
	if err := r.SetupWithManager(nil); err == nil {
		t.Fatal("expected error when Refresher is nil")
	}
}

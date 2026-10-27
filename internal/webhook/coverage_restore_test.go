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

package webhook

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// runtimeOnly implements runtime.Object but deliberately NOT client.Object
// (it has no metav1.Object accessors), so asClientObject must reject it. This
// exercises the fail-closed "object does not implement client.Object" path on
// every entrypoint that narrows a decoded admission object.
type runtimeOnly struct{}

func (r *runtimeOnly) GetObjectKind() schema.ObjectKind { return schema.EmptyObjectKind }

func (r *runtimeOnly) DeepCopyObject() runtime.Object { return &runtimeOnly{} }

var _ runtime.Object = &runtimeOnly{}

// TestAsClientObjectFailClosed covers the asClientObject error branch reached
// from Default, ValidateCreate and ValidateUpdate (both the old and new
// operands) when a decoded object is a runtime.Object but not a client.Object.
func TestAsClientObjectFailClosed(t *testing.T) {
	w := &AttributionWebhook{}
	ctx := ctxWithUser("henrik")
	bad := &runtimeOnly{}
	good := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "henrik"}, "henrik")

	t.Run("Default", func(t *testing.T) {
		if err := w.Default(ctx, bad); err == nil || !strings.Contains(err.Error(), "does not implement client.Object") {
			t.Fatalf("Default() error = %v, want client.Object narrowing failure", err)
		}
	})

	t.Run("ValidateCreate", func(t *testing.T) {
		if _, err := w.ValidateCreate(ctx, bad); err == nil || !strings.Contains(err.Error(), "does not implement client.Object") {
			t.Fatalf("ValidateCreate() error = %v, want client.Object narrowing failure", err)
		}
	})

	t.Run("ValidateUpdate old operand", func(t *testing.T) {
		if _, err := w.ValidateUpdate(ctx, bad, good); err == nil || !strings.Contains(err.Error(), "does not implement client.Object") {
			t.Fatalf("ValidateUpdate() error = %v, want client.Object narrowing failure on old", err)
		}
	})

	t.Run("ValidateUpdate new operand", func(t *testing.T) {
		if _, err := w.ValidateUpdate(ctx, good, bad); err == nil || !strings.Contains(err.Error(), "does not implement client.Object") {
			t.Fatalf("ValidateUpdate() error = %v, want client.Object narrowing failure on new", err)
		}
	})
}

// recordingSecondary is a stand-in story-1.3 cross-ref validator chained after
// attribution. It records that each path ran and can inject a failure to prove
// the chained error propagates (first failure wins).
type recordingSecondary struct {
	err                                      error
	createCalled, updateCalled, deleteCalled bool
}

func (s *recordingSecondary) ValidateCreate(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	s.createCalled = true
	return nil, s.err
}

func (s *recordingSecondary) ValidateUpdate(_ context.Context, _, _ runtime.Object) (admission.Warnings, error) {
	s.updateCalled = true
	return nil, s.err
}

func (s *recordingSecondary) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	s.deleteCalled = true
	return nil, s.err
}

var _ admission.Validator[runtime.Object] = &recordingSecondary{}

// TestTypedDefaulter covers the typedDefaulter[T] adapter: it delegates to the
// type-agnostic Default, so the attribution stamp lands through the typed path.
func TestTypedDefaulter(t *testing.T) {
	d := typedDefaulter[*fakeAttributed]{w: &AttributionWebhook{}}
	obj := newFake(nil, "")
	if err := d.Default(ctxWithUser("henrik"), obj); err != nil {
		t.Fatalf("typedDefaulter.Default() error = %v", err)
	}
	if got, ok := ksquadv1.GetCreatedBy(obj); !ok || string(got) != "henrik" {
		t.Errorf("created-by = %q (present=%v), want henrik", got, ok)
	}
	if obj.GetOwnedBy() != "henrik" {
		t.Errorf("ownedBy = %q, want henrik", obj.GetOwnedBy())
	}
}

// TestTypedValidatorNilSecondary covers the attribution-only path (secondary
// == nil, e.g. Project): each method returns the attribution verdict directly.
func TestTypedValidatorNilSecondary(t *testing.T) {
	v := typedValidator[*fakeAttributed]{w: &AttributionWebhook{}, secondary: nil}
	ctx := ctxWithUser("henrik")
	obj := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "henrik"}, "henrik")

	if _, err := v.ValidateCreate(ctx, obj); err != nil {
		t.Errorf("ValidateCreate() nil-secondary error = %v, want nil", err)
	}
	if _, err := v.ValidateUpdate(ctx, obj, obj.DeepCopyOf()); err != nil {
		t.Errorf("ValidateUpdate() nil-secondary error = %v, want nil", err)
	}
	if _, err := v.ValidateDelete(ctx, obj); err != nil {
		t.Errorf("ValidateDelete() nil-secondary error = %v, want nil", err)
	}
}

// TestTypedValidatorChainsSecondary covers the secondary-chaining branch: when
// attribution passes, each method invokes the chained cross-ref validator.
func TestTypedValidatorChainsSecondary(t *testing.T) {
	sec := &recordingSecondary{}
	v := typedValidator[*fakeAttributed]{w: &AttributionWebhook{}, secondary: sec}
	ctx := ctxWithUser("henrik")
	obj := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "henrik"}, "henrik")

	if _, err := v.ValidateCreate(ctx, obj); err != nil {
		t.Errorf("ValidateCreate() error = %v, want nil", err)
	}
	if _, err := v.ValidateUpdate(ctx, obj, obj.DeepCopyOf()); err != nil {
		t.Errorf("ValidateUpdate() error = %v, want nil", err)
	}
	if _, err := v.ValidateDelete(ctx, obj); err != nil {
		t.Errorf("ValidateDelete() error = %v, want nil", err)
	}
	if !sec.createCalled || !sec.updateCalled || !sec.deleteCalled {
		t.Errorf("secondary not chained on every path: create=%v update=%v delete=%v",
			sec.createCalled, sec.updateCalled, sec.deleteCalled)
	}
}

// TestTypedValidatorSecondaryErrorPropagates covers the secondary-failure path:
// a chained cross-ref rejection surfaces from the typed validator.
func TestTypedValidatorSecondaryErrorPropagates(t *testing.T) {
	boom := errors.New("cross-ref guard rejected")
	sec := &recordingSecondary{err: boom}
	v := typedValidator[*fakeAttributed]{w: &AttributionWebhook{}, secondary: sec}
	ctx := ctxWithUser("henrik")
	obj := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "henrik"}, "henrik")

	if _, err := v.ValidateCreate(ctx, obj); !errors.Is(err, boom) {
		t.Errorf("ValidateCreate() error = %v, want %v", err, boom)
	}
	if _, err := v.ValidateUpdate(ctx, obj, obj.DeepCopyOf()); !errors.Is(err, boom) {
		t.Errorf("ValidateUpdate() error = %v, want %v", err, boom)
	}
	if _, err := v.ValidateDelete(ctx, obj); !errors.Is(err, boom) {
		t.Errorf("ValidateDelete() error = %v, want %v", err, boom)
	}
}

// TestTypedValidatorAttributionFailsBeforeSecondary covers the early-return
// branch: when attribution itself rejects, the secondary never runs (fail
// closed on the first contract).
func TestTypedValidatorAttributionFailsBeforeSecondary(t *testing.T) {
	sec := &recordingSecondary{}
	v := typedValidator[*fakeAttributed]{w: &AttributionWebhook{}, secondary: sec}
	obj := newFake(nil, "henrik")

	// No authenticated principal → ValidateCreate fails closed before chaining.
	if _, err := v.ValidateCreate(context.Background(), obj); !errors.Is(err, ErrUnauthenticated) {
		t.Errorf("ValidateCreate() error = %v, want ErrUnauthenticated", err)
	}
	// created-by mutation → ValidateUpdate fails before chaining.
	oldObj := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "henrik"}, "henrik")
	newObj := newFake(map[string]string{ksquadv1.CreatedByAnnotation: "alice"}, "henrik")
	if _, err := v.ValidateUpdate(ctxWithUser("henrik"), oldObj, newObj); !errors.Is(err, ErrCreatedByImmutable) {
		t.Errorf("ValidateUpdate() error = %v, want ErrCreatedByImmutable", err)
	}
	if sec.createCalled || sec.updateCalled {
		t.Errorf("secondary ran despite attribution failure: create=%v update=%v", sec.createCalled, sec.updateCalled)
	}
}

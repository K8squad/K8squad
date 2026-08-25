package v1alpha1

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// DB-free unit tests for the attribution accessors and the per-type ownedBy
// holders (ISI-3213, child of ISI-2714). These are pure metadata/spec accessors
// with no Postgres dependency; they run in the ci.yml unit lane.

func TestCreatedByRoundTrip(t *testing.T) {
	obj := &Agent{}

	// Absent annotation: not present.
	if p, ok := GetCreatedBy(obj); ok || p != "" {
		t.Fatalf("GetCreatedBy on fresh obj = (%q, %v), want (\"\", false)", p, ok)
	}

	// SetCreatedBy on an object with nil annotations must allocate the map.
	SetCreatedBy(obj, PrincipalRef("user:alice"))
	p, ok := GetCreatedBy(obj)
	if !ok || p != "user:alice" {
		t.Fatalf("GetCreatedBy after set = (%q, %v), want (user:alice, true)", p, ok)
	}
	if obj.GetAnnotations()[CreatedByAnnotation] != "user:alice" {
		t.Errorf("annotation not stamped under %q", CreatedByAnnotation)
	}

	// Overwrite is permitted at the accessor level.
	SetCreatedBy(obj, PrincipalRef("user:bob"))
	if p, _ := GetCreatedBy(obj); p != "user:bob" {
		t.Errorf("GetCreatedBy after overwrite = %q, want user:bob", p)
	}
}

func TestGetCreatedByEmptyAnnotationIsAbsent(t *testing.T) {
	obj := &Agent{}
	obj.SetAnnotations(map[string]string{CreatedByAnnotation: ""})
	if p, ok := GetCreatedBy(obj); ok || p != "" {
		t.Errorf("empty annotation value = (%q, %v), want (\"\", false)", p, ok)
	}
}

func TestCreatedByIndexValue(t *testing.T) {
	obj := &Agent{}
	if v := CreatedByIndexValue(obj); v != nil {
		t.Errorf("CreatedByIndexValue on un-stamped obj = %v, want nil", v)
	}
	SetCreatedBy(obj, PrincipalRef("user:carol"))
	v := CreatedByIndexValue(obj)
	if len(v) != 1 || v[0] != "user:carol" {
		t.Errorf("CreatedByIndexValue = %v, want [user:carol]", v)
	}
}

func TestOwnedByIndexValue(t *testing.T) {
	// A holder with no owner indexes no value.
	if v := OwnedByIndexValue(&Agent{}); v != nil {
		t.Errorf("OwnedByIndexValue on ownerless holder = %v, want nil", v)
	}

	// A holder with an owner indexes that principal.
	ag := &Agent{}
	ag.SetOwnedBy(PrincipalRef("team:eng"))
	v := OwnedByIndexValue(ag)
	if len(v) != 1 || v[0] != "team:eng" {
		t.Errorf("OwnedByIndexValue = %v, want [team:eng]", v)
	}

	// A non-holder object indexes no value (does not panic).
	var nonHolder client.Object = &metav1NonHolder{}
	if v := OwnedByIndexValue(nonHolder); v != nil {
		t.Errorf("OwnedByIndexValue on non-holder = %v, want nil", v)
	}
}

func TestPerTypeOwnedByAccessors(t *testing.T) {
	const owner = PrincipalRef("user:owner")

	holders := map[string]OwnedByHolder{
		"Agent":   &Agent{},
		"Project": &Project{},
		"Run":     &Run{},
		"Team":    &Team{},
	}
	for name, h := range holders {
		if got := h.GetOwnedBy(); got != "" {
			t.Errorf("%s.GetOwnedBy() fresh = %q, want empty", name, got)
		}
		h.SetOwnedBy(owner)
		if got := h.GetOwnedBy(); got != owner {
			t.Errorf("%s.GetOwnedBy() after set = %q, want %q", name, got, owner)
		}
	}
}

// metav1NonHolder is a client.Object that does NOT implement OwnedByHolder,
// used to exercise the type-assertion guard in OwnedByIndexValue.
type metav1NonHolder struct {
	metav1.TypeMeta
	metav1.ObjectMeta
}

func (m *metav1NonHolder) DeepCopyObject() runtime.Object { return m }

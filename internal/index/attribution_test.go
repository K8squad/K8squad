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

package index

import (
	"context"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// fakeAttributed is a stand-in for the story 1.6 attributed CRDs (Team,
// Project, Agent, Run) carrying the created-by annotation + spec.ownedBy.
type fakeAttributed struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	OwnedBy ksquadv1.PrincipalRef `json:"ownedBy,omitempty"`
}

var (
	_ ksquadv1.OwnedByHolder = &fakeAttributed{}
	_ client.Object          = &fakeAttributed{}
)

func (f *fakeAttributed) GetOwnedBy() ksquadv1.PrincipalRef { return f.OwnedBy }

func (f *fakeAttributed) SetOwnedBy(p ksquadv1.PrincipalRef) { f.OwnedBy = p }

func (f *fakeAttributed) DeepCopyObject() runtime.Object {
	c := &fakeAttributed{OwnedBy: f.OwnedBy}
	c.TypeMeta = f.TypeMeta
	c.ObjectMeta = *f.DeepCopy()
	return c
}

type fakeAttributedList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []fakeAttributed `json:"items"`
}

func (l *fakeAttributedList) DeepCopyObject() runtime.Object {
	c := &fakeAttributedList{ListMeta: *l.DeepCopy()}
	for _, item := range l.Items {
		c.Items = append(c.Items, *(item.DeepCopyObject().(*fakeAttributed)))
	}
	return c
}

var fakeGVK = schema.GroupVersion{Group: "fake.ksquad.io", Version: "v1"}

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	s.AddKnownTypes(fakeGVK, &fakeAttributed{}, &fakeAttributedList{})
	metav1.AddToGroupVersion(s, fakeGVK)
	return s
}

// TestRegisterAttributionIndexes proves the story 1.6 AC "indexed for RBAC
// scope queries": after registration, a cache List scoped by created-by or
// ownedBy principal returns exactly the attributed resources.
func TestRegisterAttributionIndexes(t *testing.T) {
	ctx := context.Background()

	attributed := func(name, createdBy, ownedBy string) *fakeAttributed {
		f := &fakeAttributed{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "squad-a"},
			OwnedBy:    ksquadv1.PrincipalRef(ownedBy),
		}
		if createdBy != "" {
			f.Annotations = map[string]string{ksquadv1.CreatedByAnnotation: createdBy}
		}
		return f
	}

	c := fake.NewClientBuilder().
		WithScheme(testScheme(t)).
		WithObjects(
			attributed("team-1", "henrik", "henrik"),
			attributed("team-2", "henrik", "alice"),
			attributed("team-3", "bob", "alice"),
			attributed("team-4", "", ""), // unattributed
		).
		WithIndex(&fakeAttributed{}, ksquadv1.CreatedByIndexKey, ksquadv1.CreatedByIndexValue).
		WithIndex(&fakeAttributed{}, ksquadv1.OwnedByIndexKey, ksquadv1.OwnedByIndexValue).
		Build()

	// Sanity: registration helper accepts any FieldIndexer (the fake client
	// is one) and is idempotent-safe when given the same types it already
	// indexes for — not asserted here; the queries below prove the extractors.
	if err := RegisterAttributionIndexes(ctx, indexRecorder{}, &fakeAttributed{}); err != nil {
		t.Fatalf("RegisterAttributionIndexes() error = %v", err)
	}

	listBy := func(key, value string) []string {
		t.Helper()
		var list fakeAttributedList
		if err := c.List(ctx, &list, client.MatchingFields{key: value}); err != nil {
			t.Fatalf("List by %s=%s: %v", key, value, err)
		}
		names := []string{}
		for _, item := range list.Items {
			names = append(names, item.Name)
		}
		return names
	}

	assertSame := func(got, want []string, desc string) {
		t.Helper()
		if len(got) != len(want) {
			t.Fatalf("%s: got %v, want %v", desc, got, want)
		}
		for i := range got {
			if got[i] != want[i] {
				t.Fatalf("%s: got %v, want %v", desc, got, want)
			}
		}
	}

	// RBAC scope query: everything henrik created.
	assertSame(listBy(ksquadv1.CreatedByIndexKey, "henrik"), []string{"team-1", "team-2"}, "created-by henrik")

	// RBAC scope query: everything alice owns.
	assertSame(listBy(ksquadv1.OwnedByIndexKey, "alice"), []string{"team-2", "team-3"}, "owned-by alice")

	// Unattributed resources extract no index value: no principal matches them.
	assertSame(listBy(ksquadv1.CreatedByIndexKey, ""), []string{}, "created-by absent indexes no value")
}

// indexRecorder records IndexField calls without a real cache — proves the
// helper registers both keys for every provided type.
type indexRecorder struct{}

func (indexRecorder) IndexField(_ context.Context, _ client.Object, field string, _ client.IndexerFunc) error {
	registeredFields = append(registeredFields, field)
	return nil
}

var registeredFields []string

func TestRegisterAttributionIndexesRegistersBothKeysPerType(t *testing.T) {
	registeredFields = nil
	if err := RegisterAttributionIndexes(context.Background(), indexRecorder{}, &fakeAttributed{}); err != nil {
		t.Fatalf("RegisterAttributionIndexes() error = %v", err)
	}
	want := []string{ksquadv1.CreatedByIndexKey, ksquadv1.OwnedByIndexKey}
	if len(registeredFields) != len(want) {
		t.Fatalf("registered fields = %v, want %v", registeredFields, want)
	}
	for i := range want {
		if registeredFields[i] != want[i] {
			t.Fatalf("registered fields = %v, want %v", registeredFields, want)
		}
	}
}

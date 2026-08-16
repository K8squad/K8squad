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
	"testing"

	admissionv1 "k8s.io/api/admission/v1"
	authenticationv1 "k8s.io/api/authentication/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"

	ksquadv1 "github.com/K8squad/K8squad/api/v1alpha1"
)

// fakeAttributed is a stand-in for the story 1.6 attributed CRDs (Team,
// Project, Agent, Run): a client.Object carrying the created-by annotation
// plus a mutable spec.ownedBy via the OwnedByHolder seam.
type fakeAttributed struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	OwnedBy ksquadv1.PrincipalRef `json:"ownedBy,omitempty"`
}

var (
	_ ksquadv1.OwnedByHolder = &fakeAttributed{}
	_ runtime.Object         = &fakeAttributed{}
)

func (f *fakeAttributed) GetOwnedBy() ksquadv1.PrincipalRef { return f.OwnedBy }

func (f *fakeAttributed) SetOwnedBy(p ksquadv1.PrincipalRef) { f.OwnedBy = p }

func (f *fakeAttributed) DeepCopyObject() runtime.Object {
	c := f.DeepCopyOf()
	return c
}

// DeepCopyOf copies the fake (TypeMeta is shallow-copyable; ObjectMeta needs
// its deep copy so annotation maps are not shared).
func (f *fakeAttributed) DeepCopyOf() *fakeAttributed {
	c := &fakeAttributed{OwnedBy: f.OwnedBy}
	c.TypeMeta = f.TypeMeta
	c.ObjectMeta = *f.DeepCopy()
	return c
}

func ctxWithUser(username string) context.Context {
	return admission.NewContextWithRequest(context.Background(), admission.Request{
		AdmissionRequest: admissionv1.AdmissionRequest{
			UID: "test-uid",
			UserInfo: authenticationv1.UserInfo{
				Username: username,
			},
		},
	})
}

func newFake(annotations map[string]string, ownedBy ksquadv1.PrincipalRef) *fakeAttributed {
	return &fakeAttributed{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "test",
			Namespace:   "default",
			Annotations: annotations,
		},
		OwnedBy: ownedBy,
	}
}

func TestAttributionDefault(t *testing.T) {
	tests := []struct {
		name          string
		ctx           context.Context
		obj           *fakeAttributed
		wantCreatedBy string // "" = absent
		wantOwnedBy   ksquadv1.PrincipalRef
	}{
		{
			name:          "stamps both from the authenticated principal",
			ctx:           ctxWithUser("henrik"),
			obj:           newFake(nil, ""),
			wantCreatedBy: "henrik",
			wantOwnedBy:   "henrik",
		},
		{
			name:          "never overwrites an explicit created-by (validator rejects it)",
			ctx:           ctxWithUser("henrik"),
			obj:           newFake(map[string]string{ksquadv1.CreatedByAnnotation: "alice"}, ""),
			wantCreatedBy: "alice",
			wantOwnedBy:   "henrik",
		},
		{
			name:          "never overwrites an explicit ownedBy (ownership may be assigned)",
			ctx:           ctxWithUser("henrik"),
			obj:           newFake(nil, "alice"),
			wantCreatedBy: "henrik",
			wantOwnedBy:   "alice",
		},
		{
			name:          "no admission request in context is a no-op",
			ctx:           context.Background(),
			obj:           newFake(nil, ""),
			wantCreatedBy: "",
			wantOwnedBy:   "",
		},
	}

	w := &AttributionWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if err := w.Default(tt.ctx, tt.obj); err != nil {
				t.Fatalf("Default() error = %v", err)
			}
			got, ok := ksquadv1.GetCreatedBy(tt.obj)
			if tt.wantCreatedBy == "" && ok {
				t.Errorf("created-by annotation = %q, want absent", got)
			}
			if tt.wantCreatedBy != "" && (!ok || string(got) != tt.wantCreatedBy) {
				t.Errorf("created-by annotation = %q (present=%v), want %q", got, ok, tt.wantCreatedBy)
			}
			if tt.obj.GetOwnedBy() != tt.wantOwnedBy {
				t.Errorf("ownedBy = %q, want %q", tt.obj.GetOwnedBy(), tt.wantOwnedBy)
			}
		})
	}
}

func TestAttributionValidateCreate(t *testing.T) {
	tests := []struct {
		name      string
		ctx       context.Context
		createdAt map[string]string
		wantErr   error
	}{
		{
			name:      "defaulted created-by matching the requester is allowed",
			ctx:       ctxWithUser("henrik"),
			createdAt: map[string]string{ksquadv1.CreatedByAnnotation: "henrik"},
			wantErr:   nil,
		},
		{
			name:      "explicit created-by override for another principal is rejected",
			ctx:       ctxWithUser("henrik"),
			createdAt: map[string]string{ksquadv1.CreatedByAnnotation: "alice"},
			wantErr:   ErrCreatedByNotSettable,
		},
		{
			name:    "absent created-by passes (admission chain guarantees the stamp)",
			ctx:     ctxWithUser("henrik"),
			wantErr: nil,
		},
		{
			name:    "no authenticated principal fails closed",
			ctx:     context.Background(),
			wantErr: ErrUnauthenticated,
		},
	}

	w := &AttributionWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			obj := newFake(tt.createdAt, "henrik")
			_, err := w.ValidateCreate(tt.ctx, obj)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateCreate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func TestAttributionValidateUpdate(t *testing.T) {
	tests := []struct {
		name       string
		oldCreated string // "" = absent
		newCreated string // "" = absent
		oldOwnedBy string
		newOwnedBy string
		wantErr    error
	}{
		{
			name:       "unchanged created-by is allowed",
			oldCreated: "henrik",
			newCreated: "henrik",
			wantErr:    nil,
		},
		{
			name:       "changing created-by is rejected",
			oldCreated: "henrik",
			newCreated: "alice",
			wantErr:    ErrCreatedByImmutable,
		},
		{
			name:       "introducing created-by after creation is rejected",
			oldCreated: "",
			newCreated: "henrik",
			wantErr:    ErrCreatedByImmutable,
		},
		{
			name:       "removing created-by after it was set is rejected",
			oldCreated: "henrik",
			newCreated: "",
			wantErr:    ErrCreatedByImmutable,
		},
		{
			name:       "removing created-by is allowed only when it was never set",
			oldCreated: "",
			newCreated: "",
			wantErr:    nil,
		},
		{
			name:       "ownedBy ownership transfer is allowed",
			oldCreated: "henrik",
			newCreated: "henrik",
			oldOwnedBy: "alice",
			newOwnedBy: "bob",
			wantErr:    nil,
		},
	}

	w := &AttributionWebhook{}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			oldObj := newFake(annotationsOrNil(tt.oldCreated), ksquadv1.PrincipalRef(tt.oldOwnedBy))
			newObj := newFake(annotationsOrNil(tt.newCreated), ksquadv1.PrincipalRef(tt.newOwnedBy))
			_, err := w.ValidateUpdate(ctxWithUser("henrik"), oldObj, newObj)
			if !errors.Is(err, tt.wantErr) {
				t.Fatalf("ValidateUpdate() error = %v, want %v", err, tt.wantErr)
			}
		})
	}
}

func annotationsOrNil(principal string) map[string]string {
	if principal == "" {
		return nil
	}
	return map[string]string{ksquadv1.CreatedByAnnotation: principal}
}

func TestAttributionValidateDelete(t *testing.T) {
	w := &AttributionWebhook{}
	if _, err := w.ValidateDelete(context.Background(), newFake(nil, "henrik")); err != nil {
		t.Fatalf("ValidateDelete() error = %v, want nil", err)
	}
}

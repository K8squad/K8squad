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
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestParseCredential_Valid(t *testing.T) {
	exp := time.Date(2026, 8, 24, 20, 0, 0, 0, time.UTC)
	conn := time.Date(2026, 8, 20, 8, 0, 0, 0, time.UTC)
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Data: map[string][]byte{
			KeyRefreshToken: []byte("rt"),
			KeyExpiresAt:    []byte(exp.Format(time.RFC3339)),
			KeyConnectedAt:  []byte(conn.Format(time.RFC3339)),
		},
	}
	cs, err := parseCredential(s)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if cs.refreshToken != "rt" || !cs.expiresAt.Equal(exp) || !cs.connectedAt.Equal(conn) {
		t.Errorf("bad parse: %+v", cs)
	}
}

func TestParseCredential_MissingRefreshToken(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Data:       map[string][]byte{KeyExpiresAt: []byte(time.Now().Format(time.RFC3339))},
	}
	if _, err := parseCredential(s); err == nil {
		t.Fatal("expected error for missing refresh token")
	}
}

func TestParseCredential_MissingExpiry(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Data:       map[string][]byte{KeyRefreshToken: []byte("rt")},
	}
	if _, err := parseCredential(s); err == nil {
		t.Fatal("expected error for missing expiry")
	}
}

func TestParseCredential_BadExpiry(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Data: map[string][]byte{
			KeyRefreshToken: []byte("rt"),
			KeyExpiresAt:    []byte("not-a-time"),
		},
	}
	if _, err := parseCredential(s); err == nil {
		t.Fatal("expected error for unparseable expiry")
	}
}

func TestParseCredential_BadConnectedAtDegradesToZero(t *testing.T) {
	s := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{Name: "c", Namespace: "n"},
		Data: map[string][]byte{
			KeyRefreshToken: []byte("rt"),
			KeyExpiresAt:    []byte(time.Now().Format(time.RFC3339)),
			KeyConnectedAt:  []byte("garbage"),
		},
	}
	cs, err := parseCredential(s)
	if err != nil {
		t.Fatalf("connectedAt must be best-effort, got error: %v", err)
	}
	if !cs.connectedAt.IsZero() {
		t.Errorf("connectedAt = %v, want zero on unparseable value", cs.connectedAt)
	}
}

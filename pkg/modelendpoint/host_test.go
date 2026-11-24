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

package modelendpoint

import "testing"

// TestClassifyEndpointURL pins the shared host classification the team
// controller's BYO netpol logic and the ISI-7705 dispatch egress guard both
// key on: IP literals and in-cluster svc DNS are netpol-matchable; only a
// public FQDN is external (NetworkPolicy cannot match DNS names).
func TestClassifyEndpointURL(t *testing.T) {
	cases := []struct {
		rawURL string
		host   string
		port   int32
		ns     string
		kind   HostKind
	}{
		{"https://api.deepseek.com/v1", "api.deepseek.com", 443, "", HostExternalFQDN},
		{"http://api.example.com:8080/chat", "api.example.com", 8080, "", HostExternalFQDN},
		{"https://10.0.0.185:11434", "10.0.0.185", 11434, "", HostIP}, // LAN Ollama
		{"http://203.0.113.9", "203.0.113.9", 80, "", HostIP},         // public IP is ipBlock-matchable
		{"http://ollama.default.svc.cluster.local:11434", "ollama.default.svc.cluster.local", 11434, "default", HostInCluster},
		{"http://memory.k8squad-system.svc:8080", "memory.k8squad-system.svc", 8080, "k8squad-system", HostInCluster},
		{"not a url", "", 0, "", HostInvalid},
		{"", "", 0, "", HostInvalid},
	}
	for _, c := range cases {
		host, port, ns, kind := ClassifyEndpointURL(c.rawURL)
		if host != c.host || port != c.port || ns != c.ns || kind != c.kind {
			t.Errorf("ClassifyEndpointURL(%q) = (%q, %d, %q, %v), want (%q, %d, %q, %v)",
				c.rawURL, host, port, ns, kind, c.host, c.port, c.ns, c.kind)
		}
	}
}

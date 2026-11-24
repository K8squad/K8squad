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

import (
	"net"
	"net/url"
	"strconv"
	"strings"
)

// HostKind classifies a resolved endpoint host for egress-policy purposes.
// It is the SINGLE classification the BYO netpol logic (pkg/controller/team
// byoEgressTargetFromURL) and the dispatch-time egress guard
// (pkg/controller/rundrive) both key on (ISI-7705), so the two can never
// disagree about which hosts core NetworkPolicy can match:
//
//   - HostIP: a bare IP literal (LAN Ollama 10.0.0.185 or a public IP) —
//     matchable by an ipBlock/32 egress rule.
//   - HostInCluster: an unambiguous in-cluster Service DNS name
//     (…svc[.cluster.local]) — matchable via a namespaceSelector peer;
//     namespace carries the owning namespace.
//   - HostExternalFQDN: any other DNS name (api.deepseek.com) — core
//     NetworkPolicy CANNOT match egress by DNS name, so the only egress
//     path is the forward proxy.
//   - HostInvalid: unparseable / hostless URL.
type HostKind int

const (
	HostInvalid HostKind = iota
	HostIP
	HostInCluster
	HostExternalFQDN
)

// ClassifyEndpointURL parses a resolved endpoint BaseURL (a validated
// http(s) URL, per ResolveRef) into its host, effective port (explicit URL
// port, else the scheme default 443 https / 80 http), in-cluster namespace
// (set only for HostInCluster), and HostKind. A public FQDN is deliberately
// NOT treated as an in-cluster namespace.
func ClassifyEndpointURL(rawURL string) (host string, port int32, namespace string, kind HostKind) {
	u, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil || u.Host == "" {
		return "", 0, "", HostInvalid
	}
	host = u.Hostname()
	if ps := u.Port(); ps != "" {
		if p, err := strconv.ParseInt(ps, 10, 32); err == nil && p > 0 {
			port = int32(p)
		}
	}
	if port == 0 {
		port = 80
		if u.Scheme == "https" {
			port = 443
		}
	}
	if net.ParseIP(host) != nil {
		return host, port, "", HostIP
	}
	// In-cluster Service DNS only when unambiguous (…svc[.cluster.local]);
	// a public FQDN (api.example.com) is deliberately NOT treated as an
	// in-cluster namespace.
	labels := strings.Split(host, ".")
	for i, l := range labels {
		if l == "svc" && i >= 1 && labels[i-1] != "" {
			return host, port, labels[i-1], HostInCluster
		}
	}
	return host, port, "", HostExternalFQDN
}

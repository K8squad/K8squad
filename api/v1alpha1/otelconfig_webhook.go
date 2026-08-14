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

package v1alpha1

import (
	"context"
	"fmt"
	"net/url"
	"regexp"
	"strings"

	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/webhook/admission"
)

// SetupOTelConfigWebhookWithManager registers the OTelConfig validating
// webhook with the manager. The webhook mirrors the CRD's CEL rules (defense
// in depth for clusters where CEL surfaced errors are undesired) and adds the
// checks CEL cannot express: URL parseability, reserved resource-attribute
// keys, and attribute-key syntax.
func SetupOTelConfigWebhookWithManager(mgr ctrl.Manager) error {
	return ctrl.NewWebhookManagedBy(mgr).
		For(&OTelConfig{}).
		WithValidator(&OTelConfig{}).
		Complete()
}

// +kubebuilder:webhook:path=/validate-ksquad-io-v1alpha1-otelconfig,mutating=false,failurePolicy=fail,sideEffects=None,groups=ksquad.io,resources=otelconfigs,verbs=create;update,versions=v1alpha1,name=votelconfig-v1alpha1.ksquad.io,admissionReviewVersions=v1

var _ admission.CustomValidator = &OTelConfig{}

// ValidateCreate implements admission.CustomValidator.
func (r *OTelConfig) ValidateCreate(_ context.Context, obj runtime.Object) (admission.Warnings, error) {
	cfg, ok := obj.(*OTelConfig)
	if !ok {
		return nil, fmt.Errorf("expected an OTelConfig object but got %T", obj)
	}
	return validateOTelConfig(cfg)
}

// ValidateUpdate implements admission.CustomValidator.
func (r *OTelConfig) ValidateUpdate(_ context.Context, _, newObj runtime.Object) (admission.Warnings, error) {
	cfg, ok := newObj.(*OTelConfig)
	if !ok {
		return nil, fmt.Errorf("expected an OTelConfig object but got %T", newObj)
	}
	return validateOTelConfig(cfg)
}

// ValidateDelete implements admission.CustomValidator.
func (r *OTelConfig) ValidateDelete(_ context.Context, _ runtime.Object) (admission.Warnings, error) {
	// Deletion is always allowed.
	return nil, nil
}

// resourceAttributeKeyPattern is the OTel semantic-convention key syntax
// (unicode-free subset enforced here): dot-separated segments, each starting
// with a letter or underscore.
var resourceAttributeKeyPattern = regexp.MustCompile(
	`^[a-zA-Z_][a-zA-Z0-9_]*(\.[a-zA-Z0-9_-]+)*$`,
)

// reservedResourceAttributes are owned by the k8squad platform and must not
// be overridden by users in resourceAttributes.
var reservedResourceAttributes = map[string]struct{}{
	"service.instance.id": {},
}

// validateOTelConfig is the single validation entry point shared by create
// and update.
func validateOTelConfig(r *OTelConfig) (admission.Warnings, error) {
	var warnings admission.Warnings
	spec := r.Spec

	if spec.Traces == nil && spec.Metrics == nil && spec.Logs == nil {
		return nil, fmt.Errorf(
			"spec is empty: configure at least one of traces, metrics, logs — OTelConfig exports nothing by default (opt-in)")
	}

	signals := []struct {
		name    string
		routing *SignalRouting
	}{
		{"traces", spec.Traces},
		{"metrics", spec.Metrics},
		{"logs", spec.Logs},
	}

	for _, sig := range signals {
		if sig.routing == nil {
			continue
		}
		ws, err := validateSignalRouting(sig.name, sig.routing)
		warnings = append(warnings, ws...)
		if err != nil {
			return warnings, err
		}
	}

	return warnings, nil
}

// validateSignalRouting validates one signal block.
func validateSignalRouting(name string, routing *SignalRouting) (admission.Warnings, error) {
	var warnings admission.Warnings

	if strings.TrimSpace(routing.Endpoint) == "" {
		return nil, fmt.Errorf("spec.%s.endpoint must not be empty", name)
	}
	if strings.ContainsAny(routing.Endpoint, " \t\r\n") {
		return nil, fmt.Errorf("spec.%s.endpoint must not contain whitespace: %q", name, routing.Endpoint)
	}

	switch routing.Protocol {
	case ExportProtocolGRPC:
		// grpc: optional scheme, host, optional port, no path.
		u := routing.Endpoint
		if strings.Contains(u, "://") {
			if !strings.HasPrefix(u, "http://") && !strings.HasPrefix(u, "https://") {
				return nil, fmt.Errorf("spec.%s.endpoint scheme must be http or https for grpc: %q", name, u)
			}
			u = strings.SplitN(u, "://", 2)[1]
		}
		if strings.Contains(u, "/") {
			return nil, fmt.Errorf("spec.%s.endpoint must not contain a path for grpc: %q", name, routing.Endpoint)
		}
		if strings.HasPrefix(u, ":") {
			return nil, fmt.Errorf("spec.%s.endpoint must include a host for grpc: %q", name, routing.Endpoint)
		}
	case ExportProtocolHTTPProtobuf, ExportProtocolHTTPJSON:
		u, err := url.Parse(routing.Endpoint)
		if err != nil {
			return nil, fmt.Errorf("spec.%s.endpoint is not a parsable URL: %w", name, err)
		}
		if u.Scheme != "http" && u.Scheme != "https" {
			return nil, fmt.Errorf(
				"spec.%s.endpoint must use the http or https scheme for protocol %q: %q",
				name, routing.Protocol, routing.Endpoint)
		}
		if u.Host == "" {
			return nil, fmt.Errorf("spec.%s.endpoint must include a host: %q", name, routing.Endpoint)
		}
		if u.Scheme == "http" && !isLocalHost(u.Host) {
			warnings = append(warnings, fmt.Sprintf(
				"spec.%s.endpoint uses plaintext http to a non-local host %q; telemetry will leave the cluster unencrypted", name, u.Host))
		}
	default:
		return nil, fmt.Errorf("spec.%s.protocol %q is not one of grpc, http/protobuf, http/json", name, routing.Protocol)
	}

	if routing.Auth != nil {
		if strings.TrimSpace(routing.Auth.Name) == "" || strings.TrimSpace(routing.Auth.Key) == "" {
			return nil, fmt.Errorf("spec.%s.auth must set both name and key", name)
		}
	}

	for key, value := range routing.ResourceAttributes {
		if !resourceAttributeKeyPattern.MatchString(key) {
			return nil, fmt.Errorf(
				"spec.%s.resourceAttributes key %q is not a valid OTel attribute key (dot-separated segments starting with a letter)", name, key)
		}
		if _, reserved := reservedResourceAttributes[key]; reserved {
			return nil, fmt.Errorf(
				"spec.%s.resourceAttributes key %q is owned by the platform and cannot be overridden", name, key)
		}
		if len(value) > 1024 {
			return nil, fmt.Errorf("spec.%s.resourceAttributes[%q] value exceeds 1024 characters", name, key)
		}
	}

	if routing.Sampling != nil {
		if name != "traces" {
			return nil, fmt.Errorf("spec.%s.sampling is only valid on traces", name)
		}
		switch routing.Sampling.Type {
		case SamplingTypeProbabilistic:
			if routing.Sampling.Ratio == nil {
				return nil, fmt.Errorf("spec.%s.sampling.ratio is required when sampling.type is probabilistic", name)
			}
			if *routing.Sampling.Ratio <= 0 || *routing.Sampling.Ratio > 1 {
				return nil, fmt.Errorf("spec.%s.sampling.ratio must be in (0, 1] — use always_off to drop everything", name)
			}
		case SamplingTypeAlwaysOn, SamplingTypeAlwaysOff:
			if routing.Sampling.Ratio != nil {
				return nil, fmt.Errorf(
					"spec.%s.sampling.ratio must not be set when sampling.type is %q", name, routing.Sampling.Type)
			}
		default:
			return nil, fmt.Errorf("spec.%s.sampling.type %q is not one of always_on, always_off, probabilistic", name, routing.Sampling.Type)
		}
	}

	return warnings, nil
}

// isLocalHost reports whether host is loopback (where plaintext http is
// tolerable inside the cluster).
func isLocalHost(host string) bool {
	hostname := host
	if i := strings.LastIndex(host, ":"); i != -1 {
		hostname = host[:i]
	}
	switch hostname {
	case "localhost", "127.0.0.1", "::1", "[::1]":
		return true
	}
	return false
}

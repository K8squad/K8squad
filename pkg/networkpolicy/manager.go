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

// networkpolicy.go — enhanced network policies for team isolation (ISI-2884).
// This controller creates NetworkPolicies that enforce team boundaries and
// provide proper isolation between different teams' workloads.
package networkpolicy

import (
	"context"
	"fmt"
	"reflect"

	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	ksquadv1alpha1 "github.com/K8squad/K8squad/api/v1alpha1"
)

const (
	// LabelTeam identifies resources belonging to a team
	LabelTeam = "k8squad.io/team"
	
	// LabelApp identifies the application type
	LabelApp = "app"
	
	// PolicyTeamIsolation provides team-to-team isolation
	PolicyTeamIsolation = "team-isolation"
	
	// PolicyTeamEgress allows controlled egress traffic
	PolicyTeamEgress = "team-egress"
	
	// PolicyTeamIngress allows controlled ingress traffic within team
	PolicyTeamIngress = "team-ingress"
)

// NetworkPolicyManager manages network policy lifecycle for team isolation
type NetworkPolicyManager struct {
	client client.Client
}

// NewNetworkPolicyManager creates a new network policy manager
func NewNetworkPolicyManager(kubeClient client.Client) *NetworkPolicyManager {
	return &NetworkPolicyManager{
		client: kubeClient,
	}
}

// EnsureTeamIsolation ensures that network policies exist for the given Team.
// This creates policies that isolate the team's namespace from others.
func (npm *NetworkPolicyManager) EnsureTeamIsolation(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team isolation policy
	isolationPolicy := npm.createTeamIsolationPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &ksquadv1alpha1.NetworkPolicy{}
	err := npm.client.Get(ctx, types.NamespacedName{Name: isolationPolicy.Name, Namespace: team.Namespace}, existingPolicy)
	
	if err == nil {
		// Policy exists, check if it needs updating
		if !reflect.DeepEqual(isolationPolicy.Spec, existingPolicy.Spec) {
			logger.Info("Updating team isolation policy", "policy", isolationPolicy.Name, "team", team.Name)
			existingPolicy.Spec = isolationPolicy.Spec
			if err := npm.client.Update(ctx, existingPolicy); err != nil {
				return fmt.Errorf("failed to update team isolation policy %s: %w", isolationPolicy.Name, err)
			}
		}
		return nil
	}
	
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing network policy %s: %w", isolationPolicy.Name, err)
	}
	
	// Create the policy
	if err := npm.client.Create(ctx, isolationPolicy); err != nil {
		return fmt.Errorf("failed to create team isolation policy %s: %w", isolationPolicy.Name, err)
	}
	
	logger.Info("Created team isolation policy", "policy", isolationPolicy.Name, "team", team.Name)
	return nil
}

// EnsureTeamEgress ensures that egress network policy exists for the given Team.
// This allows controlled outbound traffic from the team's namespace.
func (npm *NetworkPolicyManager) EnsureTeamEgress(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team egress policy
	egressPolicy := npm.createTeamEgressPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &ksquadv1alpha1.NetworkPolicy{}
	err := npm.client.Get(ctx, types.NamespacedName{Name: egressPolicy.Name, Namespace: team.Namespace}, existingPolicy)
	
	if err == nil {
		// Policy exists, check if it needs updating
		if !reflect.DeepEqual(egressPolicy.Spec, existingPolicy.Spec) {
			logger.Info("Updating team egress policy", "policy", egressPolicy.Name, "team", team.Name)
			existingPolicy.Spec = egressPolicy.Spec
			if err := npm.client.Update(ctx, existingPolicy); err != nil {
				return fmt.Errorf("failed to update team egress policy %s: %w", egressPolicy.Name, err)
			}
		}
		return nil
	}
	
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing network policy %s: %w", egressPolicy.Name, err)
	}
	
	// Create the policy
	if err := npm.client.Create(ctx, egressPolicy); err != nil {
		return fmt.Errorf("failed to create team egress policy %s: %w", egressPolicy.Name, err)
	}
	
	logger.Info("Created team egress policy", "policy", egressPolicy.Name, "team", team.Name)
	return nil
}

// EnsureTeamIngress ensures that ingress network policy exists for the given Team.
// This allows controlled inbound traffic within the team's namespace.
func (npm *NetworkPolicyManager) EnsureTeamIngress(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team ingress policy
	ingressPolicy := npm.createTeamIngressPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &ksquadv1alpha1.NetworkPolicy{}
	err := npm.client.Get(ctx, types.NamespacedName{Name: ingressPolicy.Name, Namespace: team.Namespace}, existingPolicy)
	
	if err == nil {
		// Policy exists, check if it needs updating
		if !reflect.DeepEqual(ingressPolicy.Spec, existingPolicy.Spec) {
			logger.Info("Updating team ingress policy", "policy", ingressPolicy.Name, "team", team.Name)
			existingPolicy.Spec = ingressPolicy.Spec
			if err := npm.client.Update(ctx, existingPolicy); err != nil {
				return fmt.Errorf("failed to update team ingress policy %s: %w", ingressPolicy.Name, err)
			}
		}
		return nil
	}
	
	if !errors.IsNotFound(err) {
		return fmt.Errorf("failed to check existing network policy %s: %w", ingressPolicy.Name, err)
	}
	
	// Create the policy
	if err := npm.client.Create(ctx, ingressPolicy); err != nil {
		return fmt.Errorf("failed to create team ingress policy %s: %w", ingressPolicy.Name, err)
	}
	
	logger.Info("Created team ingress policy", "policy", ingressPolicy.Name, "team", team.Name)
	return nil
}

// DeleteTeamPolicies deletes all network policies for the given Team
func (npm *NetworkPolicyManager) DeleteTeamPolicies(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	policies := []string{
		fmt.Sprintf("%s-%s", PolicyTeamIsolation, team.Name),
		fmt.Sprintf("%s-%s", PolicyTeamEgress, team.Name),
		fmt.Sprintf("%s-%s", PolicyTeamIngress, team.Name),
	}
	
	for _, policyName := range policies {
		policy := &ksquadv1alpha1.NetworkPolicy{}
		err := npm.client.Get(ctx, types.NamespacedName{Name: policyName, Namespace: team.Namespace}, policy)
		
		if err != nil {
			if errors.IsNotFound(err) {
				// Policy doesn't exist, skip
				continue
			}
			return fmt.Errorf("failed to get network policy %s: %w", policyName, err)
		}
		
		// Delete the policy
		if err := npm.client.Delete(ctx, policy); err != nil {
			return fmt.Errorf("failed to delete network policy %s: %w", policyName, err)
		}
		
		logger.Info("Deleted team network policy", "policy", policyName, "team", team.Name)
	}
	
	return nil
}

// createTeamIsolationPolicy creates a network policy that isolates the team
func (npm *NetworkPolicyManager) createTeamIsolationPolicy(team *ksquadv1alpha1.Team) *ksquadv1alpha1.NetworkPolicy {
	return &ksquadv1alpha1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamIsolation, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.SchemeGroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: ksquadv1alpha1.NetworkPolicySpec{
			PolicyTypes: []ksquadv1alpha1.PolicyType{
				ksquadv1alpha1.PolicyTypeIngress,
				ksquadv1alpha1.PolicyTypeEgress,
			},
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Ingress: []ksquadv1alpha1.NetworkPolicyIngressRule{
				{
					// Allow traffic from the same team
					From: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									LabelTeam: team.Name,
								},
							},
						},
					},
					// Allow DNS queries to control plane
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 53, // DNS
							},
						},
					},
				},
				{
					// Allow health checks and metrics from control plane
					From: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 8080, // Metrics
							},
						},
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 9100, // Health checks
							},
						},
					},
				},
			},
			Egress: []ksquadv1alpha1.NetworkPolicyEgressRule{
				{
					// Allow egress to internet
					To: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							// Allow to all destinations (no selector = any IP)
						},
					},
					// Allow common outbound ports
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 443, // HTTPS
							},
						},
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 80, // HTTP
							},
						},
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 53, // DNS
							},
						},
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 22, // SSH
							},
						},
					},
				},
			},
		},
	}
}

// createTeamEgressPolicy creates a network policy that controls egress traffic
func (npm *NetworkPolicyManager) createTeamEgressPolicy(team *ksquadv1alpha1.Team) *ksquadv1alpha1.NetworkPolicy {
	return &ksquadv1alpha1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamEgress, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.SchemeGroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: ksquadv1alpha1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Egress: []ksquadv1alpha1.NetworkPolicyEgressRule{
				{
					// Allow egress to public registries for image pulls
					To: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								// Allow to all namespaces (for public registries)
							},
						},
					},
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 443, // HTTPS for registries
							},
						},
					},
				},
				{
					// Allow egress to other k8squad services within the cluster
					To: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 8080, // API server
							},
						},
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 9090, // NATS
							},
						},
					},
				},
			},
		},
	}
}

// createTeamIngressPolicy creates a network policy that controls ingress traffic
func (npm *NetworkPolicyManager) createTeamIngressPolicy(team *ksquadv1alpha1.Team) *ksquadv1alpha1.NetworkPolicy {
	return &ksquadv1alpha1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamIngress, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.SchemeGroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: ksquadv1alpha1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Ingress: []ksquadv1alpha1.NetworkPolicyIngressRule{
				{
					// Allow ingress from within the same team
					From: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									LabelTeam: team.Name,
								},
							},
						},
					},
				},
				{
					// Allow ingress from control plane components
					From: []ksquadv1alpha1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []ksquadv1alpha1.NetworkPolicyPort{
						{
							Protocol: &[]ksquadv1alpha1.Protocol{ksquadv1alpha1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 8080, // API server
							},
						},
					},
				},
			},
		},
	}
}

// ptrTo returns a pointer to the given value
func ptrTo[T any](v T) *T {
	return &v
}
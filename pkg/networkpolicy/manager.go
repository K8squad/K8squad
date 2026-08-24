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

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

// Manager manages network policy lifecycle for team isolation
type Manager struct {
	client client.Client
}

// NewNetworkPolicyManager creates a new network policy manager
func NewNetworkPolicyManager(kubeClient client.Client) *Manager {
	return &Manager{
		client: kubeClient,
	}
}

// Reconcile ensures the isolation/egress/ingress NetworkPolicies exist for each
// Team. The policies carry owner references to the Team, so deletion is handled
// by garbage collection and an absent/deleted Team is a no-op here.
func (npm *Manager) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	team := &ksquadv1alpha1.Team{}
	if err := npm.client.Get(ctx, req.NamespacedName, team); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !team.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	if err := npm.EnsureTeamIsolation(ctx, team); err != nil {
		return ctrl.Result{}, err
	}
	if err := npm.EnsureTeamEgress(ctx, team); err != nil {
		return ctrl.Result{}, err
	}
	if err := npm.EnsureTeamIngress(ctx, team); err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

// EnsureTeamIsolation ensures that network policies exist for the given Team.
// This creates policies that isolate the team's namespace from others.
func (npm *Manager) EnsureTeamIsolation(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team isolation policy
	isolationPolicy := npm.createTeamIsolationPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &networkingv1.NetworkPolicy{}
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
func (npm *Manager) EnsureTeamEgress(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team egress policy
	egressPolicy := npm.createTeamEgressPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &networkingv1.NetworkPolicy{}
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
func (npm *Manager) EnsureTeamIngress(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	// Create team ingress policy
	ingressPolicy := npm.createTeamIngressPolicy(team)
	
	// Check if policy already exists
	existingPolicy := &networkingv1.NetworkPolicy{}
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
func (npm *Manager) DeleteTeamPolicies(ctx context.Context, team *ksquadv1alpha1.Team) error {
	logger := log.FromContext(ctx)
	
	policies := []string{
		fmt.Sprintf("%s-%s", PolicyTeamIsolation, team.Name),
		fmt.Sprintf("%s-%s", PolicyTeamEgress, team.Name),
		fmt.Sprintf("%s-%s", PolicyTeamIngress, team.Name),
	}
	
	for _, policyName := range policies {
		policy := &networkingv1.NetworkPolicy{}
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
func (npm *Manager) createTeamIsolationPolicy(team *ksquadv1alpha1.Team) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamIsolation, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.GroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PolicyTypes: []networkingv1.PolicyType{
				networkingv1.PolicyTypeIngress,
				networkingv1.PolicyTypeEgress,
			},
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Allow traffic from the same team
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									LabelTeam: team.Name,
								},
							},
						},
					},
					// Allow DNS queries to control plane
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 53, // DNS
							},
						},
					},
				},
				{
					// Allow health checks and metrics from control plane
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 8080, // Metrics
							},
						},
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 9100, // Health checks
							},
						},
					},
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow egress to internet
					To: []networkingv1.NetworkPolicyPeer{
						{
							// Allow to all destinations (no selector = any IP)
						},
					},
					// Allow common outbound ports
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 443, // HTTPS
							},
						},
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 80, // HTTP
							},
						},
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 53, // DNS
							},
						},
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
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
func (npm *Manager) createTeamEgressPolicy(team *ksquadv1alpha1.Team) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamEgress, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.GroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Egress: []networkingv1.NetworkPolicyEgressRule{
				{
					// Allow egress to public registries for image pulls
					To: []networkingv1.NetworkPolicyPeer{
						{
							NamespaceSelector: &metav1.LabelSelector{
								// Allow to all namespaces (for public registries)
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 443, // HTTPS for registries
							},
						},
					},
				},
				{
					// Allow egress to other k8squad services within the cluster
					To: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
							Port: &intstr.IntOrString{
								Type:   intstr.Int,
								IntVal: 8080, // API server
							},
						},
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
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
func (npm *Manager) createTeamIngressPolicy(team *ksquadv1alpha1.Team) *networkingv1.NetworkPolicy {
	return &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      fmt.Sprintf("%s-%s", PolicyTeamIngress, team.Name),
			Namespace: team.Namespace,
			Labels: map[string]string{
				LabelTeam: team.Name,
			},
			// Owner reference for automatic cleanup when Team is deleted
			OwnerReferences: []metav1.OwnerReference{
				{
					APIVersion:         ksquadv1alpha1.GroupVersion.String(),
					Kind:               "Team",
					Name:               team.Name,
					UID:                team.UID,
					Controller:         ptrTo(true),
					BlockOwnerDeletion: ptrTo(true),
				},
			},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{
				MatchLabels: map[string]string{
					LabelTeam: team.Name,
				},
			},
			Ingress: []networkingv1.NetworkPolicyIngressRule{
				{
					// Allow ingress from within the same team
					From: []networkingv1.NetworkPolicyPeer{
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
					From: []networkingv1.NetworkPolicyPeer{
						{
							PodSelector: &metav1.LabelSelector{
								MatchLabels: map[string]string{
									"app": "k8squad-control-plane",
								},
							},
						},
					},
					Ports: []networkingv1.NetworkPolicyPort{
						{
							Protocol: &[]corev1.Protocol{corev1.ProtocolTCP}[0],
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
/*
Copyright 2021 The KubeVela Authors.

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

package policy

import (
	"context"
	"encoding/json"

	wfTypesv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
	"github.com/pkg/errors"
	utilfeature "k8s.io/apiserver/pkg/util/feature"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/features"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	"github.com/oam-dev/kubevela/pkg/policy/envbinding"
	"github.com/oam-dev/kubevela/pkg/utils"
	velaerrors "github.com/oam-dev/kubevela/pkg/utils/errors"
)

// GetClusterLabelSelectorInOCMTopology get cluster label selector in OCM topology policy spec
func GetClusterLabelSelectorInOCMTopology(topology *v1alpha1.OCMTopologyPolicySpec) map[string]string {
	if topology.ClusterLabelSelector != nil {
		return topology.ClusterLabelSelector
	}
	if utilfeature.DefaultMutableFeatureGate.Enabled(features.DeprecatedPolicySpec) {
		return topology.DeprecatedClusterSelector
	}
	return nil
}

// GetManagedClustersFromOCMTopologyPolicies resolves managed cluster names from OCM topology policies
func GetManagedClustersFromOCMTopologyPolicies(ctx context.Context, cli client.Client, policies []v1beta1.AppPolicy) ([]string, error) {
	var clusters []string
	clusterMap := make(map[string]struct{})

	addCluster := func(cluster string) {
		if _, found := clusterMap[cluster]; !found {
			clusterMap[cluster] = struct{}{}
			clusters = append(clusters, cluster)
		}
	}

	for _, policy := range policies {
		if policy.Type == v1alpha1.OCMTopologyPolicyType {
			if policy.Properties == nil {
				return nil, errors.Errorf("ocm-topology policy %s must not have empty properties", policy.Name)
			}
			topologySpec := &v1alpha1.OCMTopologyPolicySpec{}
			if err := utils.StrictUnmarshal(policy.Properties.Raw, topologySpec); err != nil {
				return nil, errors.Wrapf(err, "failed to parse ocm-topology policy %s", policy.Name)
			}
			clusterLabelSelector := GetClusterLabelSelectorInOCMTopology(topologySpec)
			switch {
			case topologySpec.Clusters != nil:
				// Validate each cluster exists as a ManagedCluster
				for _, cluster := range topologySpec.Clusters {
					managedCluster := &clusterv1.ManagedCluster{}
					if err := cli.Get(ctx, client.ObjectKey{Name: cluster}, managedCluster); err != nil {
						return nil, errors.Wrapf(err, "failed to get ManagedCluster %s", cluster)
					}
					addCluster(cluster)
				}
			case clusterLabelSelector != nil:
				// Query ManagedClusters by label selector
				managedClusterList := &clusterv1.ManagedClusterList{}
				if err := cli.List(ctx, managedClusterList, client.MatchingLabels(clusterLabelSelector)); err != nil {
					if velaerrors.IsCRDNotExists(err) {
						return nil, errors.New("ManagedCluster CRD is not installed, OCM is not available")
					}
					return nil, errors.Wrapf(err, "failed to list ManagedClusters with labels in policy %s", policy.Name)
				}
				if len(managedClusterList.Items) == 0 && !topologySpec.AllowEmpty {
					return nil, errors.New("failed to find any ManagedCluster matching given labels")
				}
				for _, mc := range managedClusterList.Items {
					addCluster(mc.Name)
				}
			default:
				return nil, errors.Errorf("ocm-topology policy %s must specify either clusters or clusterLabelSelector", policy.Name)
			}
		}
	}
	return clusters, nil
}

// GetOCMTopologyPolicySpec returns the OCM topology policy spec from policies
func GetOCMTopologyPolicySpec(policies []v1beta1.AppPolicy) (*v1alpha1.OCMTopologyPolicySpec, error) {
	for _, policy := range policies {
		if policy.Type == v1alpha1.OCMTopologyPolicyType {
			if policy.Properties == nil {
				return nil, errors.Errorf("ocm-topology policy %s must not have empty properties", policy.Name)
			}
			topologySpec := &v1alpha1.OCMTopologyPolicySpec{}
			if err := utils.StrictUnmarshal(policy.Properties.Raw, topologySpec); err != nil {
				return nil, errors.Wrapf(err, "failed to parse ocm-topology policy %s", policy.Name)
			}
			return topologySpec, nil
		}
	}
	return nil, nil
}

// BuildWrappedApplication creates an Application with filtered policies for wrapping in ManifestWork
// It removes ocm-topology and override policies, keeping all other policies, traits, and workflow steps
func BuildWrappedApplication(app *v1beta1.Application, components []common.ApplicationComponent, targetNamespace string) *v1beta1.Application {
	// Filter policies - remove ocm-topology and override
	var filteredPolicies []v1beta1.AppPolicy
	for _, policy := range app.Spec.Policies {
		if policy.Type != v1alpha1.OCMTopologyPolicyType && policy.Type != v1alpha1.OverridePolicyType {
			filteredPolicies = append(filteredPolicies, policy)
		}
	}

	// Determine target namespace
	ns := targetNamespace
	if ns == "" {
		ns = app.Namespace
	}

	// Build wrapped application
	wrappedApp := &v1beta1.Application{}
	wrappedApp.SetGroupVersionKind(v1beta1.ApplicationKindVersionKind)
	wrappedApp.SetName(app.Name)
	wrappedApp.SetNamespace(ns)
	wrappedApp.SetLabels(app.Labels)
	wrappedApp.SetAnnotations(app.Annotations)

	wrappedApp.Spec.Components = components
	wrappedApp.Spec.Policies = filteredPolicies

	// Copy and filter workflow - remove ocm-topology and override policy references from steps
	// since these policies are already applied on the hub
	if app.Spec.Workflow != nil {
		wrappedApp.Spec.Workflow = filterWorkflowPolicies(app.Spec.Workflow.DeepCopy(), app.Spec.Policies)
	}

	return wrappedApp
}

// ApplyOverridePolicies applies override policies to components
func ApplyOverridePolicies(policies []v1beta1.AppPolicy, components []common.ApplicationComponent) ([]common.ApplicationComponent, error) {
	var err error
	for _, policy := range policies {
		if policy.Type == v1alpha1.OverridePolicyType {
			if policy.Properties == nil {
				return nil, errors.Errorf("override policy %s must not have empty properties", policy.Name)
			}
			overrideSpec := &v1alpha1.OverridePolicySpec{}
			if err := utils.StrictUnmarshal(policy.Properties.Raw, overrideSpec); err != nil {
				return nil, errors.Wrapf(err, "failed to parse override policy %s", policy.Name)
			}
			components, err = envbinding.PatchComponents(components, overrideSpec.Components, overrideSpec.Selector)
			if err != nil {
				return nil, errors.Wrapf(err, "failed to apply override policy %s", policy.Name)
			}
		}
	}
	return components, nil
}

// HasOCMTopologyPolicy checks if any of the policies is an OCM topology policy
func HasOCMTopologyPolicy(policies []v1beta1.AppPolicy) bool {
	for _, policy := range policies {
		if policy.Type == v1alpha1.OCMTopologyPolicyType {
			return true
		}
	}
	return false
}

// filterWorkflowPolicies filters workflow steps to remove references to ocm-topology and override policies
// since these policies are already applied on the hub. Steps that have no remaining policies are removed.
func filterWorkflowPolicies(workflow *v1beta1.Workflow, policies []v1beta1.AppPolicy) *v1beta1.Workflow {
	if workflow == nil {
		return nil
	}

	// Build a set of policy names that should be filtered out (ocm-topology and override)
	filteredPolicyNames := make(map[string]struct{})
	for _, policy := range policies {
		if policy.Type == v1alpha1.OCMTopologyPolicyType || policy.Type == v1alpha1.OverridePolicyType {
			filteredPolicyNames[policy.Name] = struct{}{}
		}
	}

	// If no policies to filter, return as-is
	if len(filteredPolicyNames) == 0 {
		return workflow
	}

	// Filter workflow steps
	var filteredSteps []wfTypesv1alpha1.WorkflowStep
	for _, step := range workflow.Steps {
		filteredStep := filterStepPolicies(step, filteredPolicyNames)
		if filteredStep != nil {
			filteredSteps = append(filteredSteps, *filteredStep)
		}
	}

	workflow.Steps = filteredSteps
	return workflow
}

// filterStepPolicies filters a single workflow step to remove references to filtered policy names
// Returns nil if the step should be removed (no remaining policies)
func filterStepPolicies(step wfTypesv1alpha1.WorkflowStep, filteredPolicyNames map[string]struct{}) *wfTypesv1alpha1.WorkflowStep {
	if step.Properties == nil || len(step.Properties.Raw) == 0 {
		return &step
	}

	// Parse the properties to extract policies
	var props map[string]interface{}
	if err := json.Unmarshal(step.Properties.Raw, &props); err != nil {
		// If we can't parse, keep the step as-is
		return &step
	}

	policiesRaw, hasPolicies := props["policies"]
	if !hasPolicies {
		return &step
	}

	// Extract and filter policies
	policiesList, ok := policiesRaw.([]interface{})
	if !ok {
		return &step
	}

	var remainingPolicies []string
	for _, p := range policiesList {
		policyName, ok := p.(string)
		if !ok {
			continue
		}
		if _, shouldFilter := filteredPolicyNames[policyName]; !shouldFilter {
			remainingPolicies = append(remainingPolicies, policyName)
		}
	}

	// If no policies remain, remove the step entirely
	if len(remainingPolicies) == 0 {
		return nil
	}

	// Update the step with filtered policies
	props["policies"] = remainingPolicies
	step.Properties = util.Object2RawExtension(props)

	// Recursively filter sub-steps if present
	if len(step.SubSteps) > 0 {
		var filteredSubSteps []wfTypesv1alpha1.WorkflowStepBase
		for _, subStep := range step.SubSteps {
			// Create a WorkflowStep to reuse the filter function
			wrappedSubStep := wfTypesv1alpha1.WorkflowStep{WorkflowStepBase: subStep}
			filteredSubStep := filterStepPolicies(wrappedSubStep, filteredPolicyNames)
			if filteredSubStep != nil {
				filteredSubSteps = append(filteredSubSteps, filteredSubStep.WorkflowStepBase)
			}
		}
		step.SubSteps = filteredSubSteps
	}

	return &step
}

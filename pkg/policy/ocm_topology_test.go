/*
Copyright 2022 The KubeVela Authors.

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
	"testing"

	wfTypesv1alpha1 "github.com/kubevela/pkg/apis/oam/v1alpha1"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/oam/util"
	velacommon "github.com/oam-dev/kubevela/pkg/utils/common"
)

func TestGetManagedClustersFromOCMTopologyPolicies(t *testing.T) {
	scheme := velacommon.Scheme
	cli := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
		&clusterv1.ManagedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster-a",
				Labels: map[string]string{
					"env": "prod",
				},
			},
			Spec: clusterv1.ManagedClusterSpec{
				HubAcceptsClient: true,
				ManagedClusterClientConfigs: []clusterv1.ClientConfig{
					{URL: "https://cluster-a.example.com"},
				},
			},
		},
		&clusterv1.ManagedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster-b",
				Labels: map[string]string{
					"env": "prod",
				},
			},
			Spec: clusterv1.ManagedClusterSpec{
				HubAcceptsClient: true,
				ManagedClusterClientConfigs: []clusterv1.ClientConfig{
					{URL: "https://cluster-b.example.com"},
				},
			},
		},
		&clusterv1.ManagedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster-c",
				Labels: map[string]string{
					"env": "dev",
				},
			},
			Spec: clusterv1.ManagedClusterSpec{
				HubAcceptsClient: true,
				ManagedClusterClientConfigs: []clusterv1.ClientConfig{
					{URL: "https://cluster-c.example.com"},
				},
			},
		},
	).Build()

	testCases := map[string]struct {
		Inputs  []v1beta1.AppPolicy
		Outputs []string
		Error   string
	}{
		"invalid-ocm-topology-policy": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"cluster":"x"}`)},
			}},
			Error: "failed to parse ocm-topology policy",
		},
		"cluster-not-found": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-x"]}`)},
			}},
			Error: "failed to get ManagedCluster",
		},
		"ocm-topology-by-clusters": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a"]}`)},
			}},
			Outputs: []string{"cluster-a"},
		},
		"ocm-topology-by-multiple-clusters": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a","cluster-b"]}`)},
			}},
			Outputs: []string{"cluster-a", "cluster-b"},
		},
		"ocm-topology-by-cluster-label-selector-404": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusterLabelSelector":{"env":"staging"}}`)},
			}},
			Error: "failed to find any ManagedCluster matching given labels",
		},
		"ocm-topology-by-cluster-label-selector-ignore-404": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusterLabelSelector":{"env":"staging"},"allowEmpty":true}`)},
			}},
			Outputs: nil,
		},
		"ocm-topology-by-cluster-label-selector": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"clusterLabelSelector":{"env":"prod"}}`)},
			}},
			Outputs: []string{"cluster-a", "cluster-b"},
		},
		"no-ocm-topology-policy": {
			Inputs:  []v1beta1.AppPolicy{},
			Outputs: nil,
		},
		"empty-ocm-topology-policy": {
			Inputs: []v1beta1.AppPolicy{{Type: v1alpha1.OCMTopologyPolicyType, Name: "some-name", Properties: nil}},
			Error:  "must not have empty properties",
		},
		"ocm-topology-no-clusters-or-selector": {
			Inputs: []v1beta1.AppPolicy{{
				Name:       "ocm-topology-policy",
				Type:       v1alpha1.OCMTopologyPolicyType,
				Properties: &runtime.RawExtension{Raw: []byte(`{"namespace":"override"}`)},
			}},
			Error: "must specify either clusters or clusterLabelSelector",
		},
	}

	for name, tt := range testCases {
		t.Run(name, func(t *testing.T) {
			r := require.New(t)
			clusters, err := GetManagedClustersFromOCMTopologyPolicies(context.Background(), cli, tt.Inputs)
			if tt.Error != "" {
				r.NotNil(err)
				r.Contains(err.Error(), tt.Error)
			} else {
				r.NoError(err)
				r.Equal(tt.Outputs, clusters)
			}
		})
	}
}

func TestBuildWrappedApplication(t *testing.T) {
	r := require.New(t)

	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
			Labels: map[string]string{
				"app": "test",
			},
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []common.ApplicationComponent{
				{
					Name: "nginx",
					Type: "webservice",
					Properties: &runtime.RawExtension{
						Raw: []byte(`{"image":"nginx:latest"}`),
					},
				},
			},
			Policies: []v1beta1.AppPolicy{
				{
					Name:       "ocm-deploy",
					Type:       v1alpha1.OCMTopologyPolicyType,
					Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a"]}`)},
				},
				{
					Name:       "override-prod",
					Type:       v1alpha1.OverridePolicyType,
					Properties: &runtime.RawExtension{Raw: []byte(`{"components":[{"name":"nginx","properties":{"replicas":3}}]}`)},
				},
				{
					Name:       "debug-policy",
					Type:       v1alpha1.DebugPolicyType,
					Properties: &runtime.RawExtension{Raw: []byte(`{}`)},
				},
			},
		},
	}

	components := []common.ApplicationComponent{
		{
			Name: "nginx",
			Type: "webservice",
			Properties: &runtime.RawExtension{
				Raw: []byte(`{"image":"nginx:latest","replicas":3}`),
			},
		},
	}

	// Test without target namespace
	wrappedApp := BuildWrappedApplication(app, components, "")
	r.Equal("test-app", wrappedApp.Name)
	r.Equal("default", wrappedApp.Namespace)
	r.Equal(1, len(wrappedApp.Spec.Components))
	r.Equal("nginx", wrappedApp.Spec.Components[0].Name)
	// Should have only debug-policy (ocm-topology and override filtered out)
	r.Equal(1, len(wrappedApp.Spec.Policies))
	r.Equal("debug-policy", wrappedApp.Spec.Policies[0].Name)
	r.Equal(v1alpha1.DebugPolicyType, wrappedApp.Spec.Policies[0].Type)

	// Test with target namespace
	wrappedApp = BuildWrappedApplication(app, components, "prod-ns")
	r.Equal("prod-ns", wrappedApp.Namespace)
}

func TestApplyOverridePolicies(t *testing.T) {
	r := require.New(t)

	components := []common.ApplicationComponent{
		{
			Name: "nginx",
			Type: "webservice",
			Properties: &runtime.RawExtension{
				Raw: []byte(`{"image":"nginx:1.19"}`),
			},
		},
	}

	policies := []v1beta1.AppPolicy{
		{
			Name:       "override-prod",
			Type:       v1alpha1.OverridePolicyType,
			Properties: &runtime.RawExtension{Raw: []byte(`{"components":[{"name":"nginx","properties":{"image":"nginx:1.20"}}]}`)},
		},
	}

	result, err := ApplyOverridePolicies(policies, components)
	r.NoError(err)
	r.Equal(1, len(result))
	r.Contains(string(result[0].Properties.Raw), "nginx:1.20")
}

func TestHasOCMTopologyPolicy(t *testing.T) {
	r := require.New(t)

	policies := []v1beta1.AppPolicy{
		{Name: "topology", Type: v1alpha1.TopologyPolicyType},
		{Name: "override", Type: v1alpha1.OverridePolicyType},
	}
	r.False(HasOCMTopologyPolicy(policies))

	policies = append(policies, v1beta1.AppPolicy{
		Name: "ocm-topology",
		Type: v1alpha1.OCMTopologyPolicyType,
	})
	r.True(HasOCMTopologyPolicy(policies))
}

func TestGetOCMTopologyPolicySpec(t *testing.T) {
	r := require.New(t)

	policies := []v1beta1.AppPolicy{
		{Name: "topology", Type: v1alpha1.TopologyPolicyType},
	}
	spec, err := GetOCMTopologyPolicySpec(policies)
	r.NoError(err)
	r.Nil(spec)

	policies = []v1beta1.AppPolicy{
		{
			Name:       "ocm-topology",
			Type:       v1alpha1.OCMTopologyPolicyType,
			Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a"],"namespace":"prod"}`)},
		},
	}
	spec, err = GetOCMTopologyPolicySpec(policies)
	r.NoError(err)
	r.NotNil(spec)
	r.Equal([]string{"cluster-a"}, spec.Clusters)
	r.Equal("prod", spec.Namespace)
}

func TestFilterWorkflowPolicies(t *testing.T) {
	r := require.New(t)

	policies := []v1beta1.AppPolicy{
		{Name: "ocm-topology-1", Type: v1alpha1.OCMTopologyPolicyType},
		{Name: "override-prod", Type: v1alpha1.OverridePolicyType},
		{Name: "debug-policy", Type: v1alpha1.DebugPolicyType},
		{Name: "gc-policy", Type: v1alpha1.GarbageCollectPolicyType},
	}

	// Test: Step with only ocm-topology and override policies should be removed
	t.Run("remove step with only filtered policies", func(t *testing.T) {
		workflow := &v1beta1.Workflow{
			Steps: []wfTypesv1alpha1.WorkflowStep{
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy-ocm",
						Type:       "deploy-ocm",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"ocm-topology-1", "override-prod"}}),
					},
				},
			},
		}

		result := filterWorkflowPolicies(workflow, policies)
		r.Equal(0, len(result.Steps), "step with only ocm-topology and override policies should be removed")
	})

	// Test: Step with mixed policies should keep non-filtered policies
	t.Run("keep step with mixed policies", func(t *testing.T) {
		workflow := &v1beta1.Workflow{
			Steps: []wfTypesv1alpha1.WorkflowStep{
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy-step",
						Type:       "deploy",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"ocm-topology-1", "debug-policy", "gc-policy"}}),
					},
				},
			},
		}

		result := filterWorkflowPolicies(workflow, policies)
		r.Equal(1, len(result.Steps), "step should be kept")
		r.Equal("deploy-step", result.Steps[0].Name)
		// Verify policies are filtered
		var props map[string]interface{}
		r.NoError(json.Unmarshal(result.Steps[0].Properties.Raw, &props))
		remainingPolicies := props["policies"].([]interface{})
		r.Equal(2, len(remainingPolicies))
		r.Contains(remainingPolicies, "debug-policy")
		r.Contains(remainingPolicies, "gc-policy")
	})

	// Test: Step without policies property should be kept as-is
	t.Run("keep step without policies property", func(t *testing.T) {
		workflow := &v1beta1.Workflow{
			Steps: []wfTypesv1alpha1.WorkflowStep{
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "apply-component",
						Type:       "apply-component",
						Properties: util.Object2RawExtension(map[string]interface{}{"component": "nginx"}),
					},
				},
			},
		}

		result := filterWorkflowPolicies(workflow, policies)
		r.Equal(1, len(result.Steps))
		r.Equal("apply-component", result.Steps[0].Name)
	})

	// Test: Nil workflow should return nil
	t.Run("nil workflow returns nil", func(t *testing.T) {
		result := filterWorkflowPolicies(nil, policies)
		r.Nil(result)
	})

	// Test: No policies to filter returns workflow as-is
	t.Run("no policies to filter", func(t *testing.T) {
		workflow := &v1beta1.Workflow{
			Steps: []wfTypesv1alpha1.WorkflowStep{
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy",
						Type:       "deploy",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"debug-policy"}}),
					},
				},
			},
		}

		noPolicies := []v1beta1.AppPolicy{
			{Name: "debug-policy", Type: v1alpha1.DebugPolicyType},
		}

		result := filterWorkflowPolicies(workflow, noPolicies)
		r.Equal(1, len(result.Steps))
	})

	// Test: Multiple steps - some removed, some kept
	t.Run("multiple steps filtering", func(t *testing.T) {
		workflow := &v1beta1.Workflow{
			Steps: []wfTypesv1alpha1.WorkflowStep{
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy-ocm-1",
						Type:       "deploy-ocm",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"ocm-topology-1"}}),
					},
				},
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy-normal",
						Type:       "deploy",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"debug-policy", "gc-policy"}}),
					},
				},
				{
					WorkflowStepBase: wfTypesv1alpha1.WorkflowStepBase{
						Name:       "deploy-mixed",
						Type:       "deploy",
						Properties: util.Object2RawExtension(map[string]interface{}{"policies": []string{"override-prod", "gc-policy"}}),
					},
				},
			},
		}

		result := filterWorkflowPolicies(workflow, policies)
		r.Equal(2, len(result.Steps), "first step should be removed, second and third kept")
		r.Equal("deploy-normal", result.Steps[0].Name)
		r.Equal("deploy-mixed", result.Steps[1].Name)
	})
}

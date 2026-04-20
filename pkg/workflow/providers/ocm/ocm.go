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

package ocm

import (
	"context"
	_ "embed"

	cuexruntime "github.com/kubevela/pkg/cue/cuex/runtime"
	"github.com/pkg/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	pkgpolicy "github.com/oam-dev/kubevela/pkg/policy"
	oamprovidertypes "github.com/oam-dev/kubevela/pkg/workflow/providers/types"
)

// DeployOCMParameter is the parameter for deploy-ocm workflow step
type DeployOCMParameter struct {
	// Policies specifies the policy names used for this deployment
	Policies []string `json:"policies,omitempty"`
	// Parallelism is the maximum number of concurrent deployments
	Parallelism int64 `json:"parallelism"`
}

// DeployOCMParams is the params for deploy-ocm
type DeployOCMParams = oamprovidertypes.Params[DeployOCMParameter]

// DeployOCM deploys the application to OCM managed clusters via ManifestWork
func DeployOCM(ctx context.Context, params *DeployOCMParams) (*any, error) {
	if params.Params.Parallelism <= 0 {
		params.Params.Parallelism = 5
	}
	// Get the dispatcher from KubeHandlers to properly track resources
	// This ensures ManifestWorks are recorded in ResourceTracker and not deleted by GC
	var dispatcher func(ctx context.Context, client client.Client, cluster, owner string, manifests ...*unstructured.Unstructured) error
	if params.KubeHandlers != nil && params.KubeHandlers.Apply != nil {
		dispatcher = params.KubeHandlers.Apply
	} else {
		return nil, errors.New("KubeHandlers.Apply dispatcher is required for OCM deployment")
	}
	executor := NewOCMDeployExecutor(
		params.KubeClient,
		params.Appfile,
		params.App,
		params.Params,
		dispatcher,
	)
	healthy, reason, err := executor.Deploy(ctx)
	if err != nil {
		return nil, err
	}
	if !healthy {
		params.Action.Wait(reason)
	}
	return nil, nil
}

// ListManagedClustersParams is the params for listing managed clusters
type ListManagedClustersParams struct {
	Policies []string `json:"policies"`
}

// ListManagedClustersResult is the result for listing managed clusters
type ListManagedClustersResult struct {
	Clusters []string `json:"clusters"`
}

// ListManagedClustersReturns is the return type for listing managed clusters
type ListManagedClustersReturns = oamprovidertypes.Returns[ListManagedClustersResult]

// ListManagedClusters lists managed clusters based on the OCM topology policies
func ListManagedClusters(ctx context.Context, params *oamprovidertypes.Params[ListManagedClustersParams]) (*ListManagedClustersReturns, error) {
	policies, err := selectPolicies(params.Appfile.Policies, params.Params.Policies)
	if err != nil {
		return nil, err
	}
	clusters, err := pkgpolicy.GetManagedClustersFromOCMTopologyPolicies(ctx, params.KubeClient, policies)
	if err != nil {
		return nil, err
	}
	return &ListManagedClustersReturns{Returns: ListManagedClustersResult{Clusters: clusters}}, nil
}

func selectPolicies(policies []v1beta1.AppPolicy, policyNames []string) ([]v1beta1.AppPolicy, error) {
	if len(policyNames) == 0 {
		return policies, nil
	}
	policyMap := make(map[string]v1beta1.AppPolicy)
	for _, policy := range policies {
		policyMap[policy.Name] = policy
	}
	var selectedPolicies []v1beta1.AppPolicy
	for _, policyName := range policyNames {
		if policy, found := policyMap[policyName]; found {
			selectedPolicies = append(selectedPolicies, policy)
		}
	}
	return selectedPolicies, nil
}

//go:embed ocm.cue
var template string

// GetTemplate returns the cue template.
func GetTemplate() string {
	return template
}

// GetProviders returns the cue providers.
func GetProviders() map[string]cuexruntime.ProviderFn {
	return map[string]cuexruntime.ProviderFn{
		"deploy-ocm":            oamprovidertypes.GenericProviderFn[DeployOCMParameter, any](DeployOCM),
		"list-managed-clusters": oamprovidertypes.GenericProviderFn[ListManagedClustersParams, ListManagedClustersReturns](ListManagedClusters),
	}
}

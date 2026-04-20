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
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"sync"

	"github.com/kubevela/pkg/util/slices"
	providertypes "github.com/kubevela/workflow/pkg/providers/types"
	"github.com/pkg/errors"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ocmworkv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/oam"
	pkgpolicy "github.com/oam-dev/kubevela/pkg/policy"
)

// OCMDeployExecutor executes OCM deployment
type OCMDeployExecutor interface {
	Deploy(ctx context.Context) (healthy bool, reason string, err error)
}

// NewOCMDeployExecutor creates a new OCM deploy executor
func NewOCMDeployExecutor(cli client.Client, af *appfile.Appfile, app *v1beta1.Application, parameter DeployOCMParameter, dispatcher providertypes.Dispatcher) OCMDeployExecutor {
	return &ocmDeployExecutor{
		cli:        cli,
		af:         af,
		app:        app,
		parameter:  parameter,
		dispatcher: dispatcher,
	}
}

type ocmDeployExecutor struct {
	cli        client.Client
	af         *appfile.Appfile
	app        *v1beta1.Application
	parameter  DeployOCMParameter
	dispatcher providertypes.Dispatcher
}

// clusterDeployResult holds the result of deploying to a single cluster
type clusterDeployResult struct {
	cluster string
	healthy bool
	message string
	err     error
}

// Deploy executes the OCM deployment workflow step
func (e *ocmDeployExecutor) Deploy(ctx context.Context) (bool, string, error) {
	// Select policies based on parameter
	policies, err := selectPolicies(e.af.Policies, e.parameter.Policies)
	if err != nil {
		return false, "", err
	}

	// Get managed clusters from OCM topology policies
	clusters, err := pkgpolicy.GetManagedClustersFromOCMTopologyPolicies(ctx, e.cli, policies)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to get managed clusters from OCM topology policies")
	}

	if len(clusters) == 0 {
		return true, "", nil
	}

	// Get OCM topology policy spec for namespace configuration
	topologySpec, err := pkgpolicy.GetOCMTopologyPolicySpec(policies)
	if err != nil {
		return false, "", err
	}

	// Apply override policies to components
	components := make([]common.ApplicationComponent, len(e.app.Spec.Components))
	for i, comp := range e.app.Spec.Components {
		components[i] = *comp.DeepCopy()
	}
	components, err = pkgpolicy.ApplyOverridePolicies(policies, components)
	if err != nil {
		return false, "", errors.Wrap(err, "failed to apply override policies")
	}

	// Determine target namespace
	targetNamespace := ""
	if topologySpec != nil {
		targetNamespace = topologySpec.Namespace
	}
	if targetNamespace == "" {
		targetNamespace = e.app.Namespace
	}

	// Build wrapped application
	wrappedApp := pkgpolicy.BuildWrappedApplication(e.app, components, targetNamespace)

	// Deploy to each managed cluster in parallel
	results := slices.ParMap(clusters, func(cluster string) clusterDeployResult {
		err := e.deployToCluster(ctx, cluster, wrappedApp)
		if err != nil {
			return clusterDeployResult{cluster: cluster, healthy: false, message: err.Error(), err: err}
		}
		// Check if the ManifestWork is applied
		healthy, message, err := e.checkManifestWorkHealth(ctx, cluster)
		return clusterDeployResult{cluster: cluster, healthy: healthy, message: message, err: err}
	}, slices.Parallelism(int(e.parameter.Parallelism)))

	// Build cluster health map from results
	clusterHealthMap := make(map[string]clusterDeployResult)
	for _, res := range results {
		clusterHealthMap[res.cluster] = res
	}

	// Update service status for each component on each cluster
	e.updateServiceStatus(components, clusters, targetNamespace, clusterHealthMap)

	// Aggregate results
	var errs []error
	var reasons []string
	allHealthy := true
	for _, res := range results {
		if res.err != nil {
			errs = append(errs, errors.Wrapf(res.err, "failed to deploy to cluster %s", res.cluster))
		}
		if !res.healthy {
			allHealthy = false
			reasons = append(reasons, fmt.Sprintf("cluster %s is not ready", res.cluster))
		}
	}

	if len(errs) > 0 {
		return false, strings.Join(reasons, ", "), errors.Errorf("deployment errors: %v", errs)
	}

	return allHealthy, strings.Join(reasons, ", "), nil
}

func (e *ocmDeployExecutor) deployToCluster(ctx context.Context, clusterName string, wrappedApp *v1beta1.Application) error {
	manifestWork, err := e.buildManifestWork(clusterName, wrappedApp)
	if err != nil {
		klog.ErrorS(err, "Failed to build ManifestWork", "cluster", clusterName, "app", e.app.Name)
		return err
	}

	klog.InfoS("Deploying to cluster", "cluster", clusterName, "manifestWorkName", manifestWork.Name, "app", e.app.Name)

	// Convert ManifestWork to unstructured for dispatch
	// This allows ResourceKeeper to track the resource and prevent GC
	unstructuredMW, err := runtime.DefaultUnstructuredConverter.ToUnstructured(manifestWork)
	if err != nil {
		klog.ErrorS(err, "Failed to convert ManifestWork to unstructured", "cluster", clusterName, "manifestWorkName", manifestWork.Name)
		return errors.Wrap(err, "failed to convert ManifestWork to unstructured")
	}

	mwObj := &unstructured.Unstructured{Object: unstructuredMW}
	mwObj.SetGroupVersionKind(ocmworkv1.GroupVersion.WithKind("ManifestWork"))

	// Use the dispatcher to apply and track the ManifestWork
	// The dispatcher uses ResourceKeeper internally which:
	// 1. Creates/updates the resource
	// 2. Records it in ResourceTracker to prevent GC from deleting it
	klog.InfoS("Dispatching ManifestWork", "cluster", clusterName, "manifestWorkName", manifestWork.Name,
		"appName", e.app.Name, "appNamespace", e.app.Namespace, "appGeneration", e.app.Generation,
		"mwNamespace", mwObj.GetNamespace(), "mwLabels", mwObj.GetLabels())
	if err := e.dispatcher(ctx, e.cli, "", common.WorkflowResourceCreator, mwObj); err != nil {
		klog.ErrorS(err, "Failed to dispatch ManifestWork", "cluster", clusterName, "manifestWorkName", manifestWork.Name)
		return err
	}

	klog.InfoS("Successfully dispatched ManifestWork", "cluster", clusterName, "manifestWorkName", manifestWork.Name)
	return nil
}

func (e *ocmDeployExecutor) buildManifestWork(clusterName string, wrappedApp *v1beta1.Application) (*ocmworkv1.ManifestWork, error) {
	// Marshal the wrapped application
	// NOTE: The wrappedApp has already been processed by pkgpolicy.BuildWrappedApplication which:
	// 1. Removes ocm-topology and override policies (already applied on hub)
	// 2. Filters workflow steps to remove references to ocm-topology and override policies
	//    (steps with only those policies are removed entirely)
	// 3. Applies the target namespace from the ocm-topology policy
	appBytes, err := json.Marshal(wrappedApp)
	if err != nil {
		return nil, errors.Wrap(err, "failed to marshal wrapped application")
	}

	// Generate a hash for the manifest work name
	hash := sha256.Sum256([]byte(e.app.Name + e.app.Namespace))
	shortHash := hex.EncodeToString(hash[:])[:8]

	manifestWorkName := fmt.Sprintf("%s-%s", e.app.Name, shortHash)
	// ManifestWork name must be DNS-1123 compliant
	manifestWorkName = strings.ToLower(manifestWorkName)
	if len(manifestWorkName) > 63 {
		manifestWorkName = manifestWorkName[:63]
	}

	// Build labels for KubeVela resource tracking
	// These labels allow KubeVela's ResourceTracker to manage the lifecycle of this resource
	labels := map[string]string{
		oam.LabelAppName:      e.app.Name,
		oam.LabelAppNamespace: e.app.Namespace,
	}

	// NOTE: We do NOT set OwnerReferences because:
	// 1. ManifestWork is in namespace=clusterName (e.g., "spoke2") on the hub
	// 2. Application is in a different namespace (e.g., "examples")
	// 3. Cross-namespace owner references cause OCM to delete the ManifestWork
	// 4. KubeVela's ResourceTracker handles lifecycle management via labels instead

	manifestWork := &ocmworkv1.ManifestWork{
		ObjectMeta: metav1.ObjectMeta{
			Name:      manifestWorkName,
			Namespace: clusterName,
			Labels:    labels,
		},
		Spec: ocmworkv1.ManifestWorkSpec{
			Workload: ocmworkv1.ManifestsTemplate{
				Manifests: []ocmworkv1.Manifest{
					{
						RawExtension: runtime.RawExtension{Raw: appBytes},
					},
				},
			},
		},
	}

	return manifestWork, nil
}

func (e *ocmDeployExecutor) checkManifestWorkHealth(ctx context.Context, clusterName string) (bool, string, error) {
	hash := sha256.Sum256([]byte(e.app.Name + e.app.Namespace))
	shortHash := hex.EncodeToString(hash[:])[:8]
	manifestWorkName := fmt.Sprintf("%s-%s", e.app.Name, shortHash)
	manifestWorkName = strings.ToLower(manifestWorkName)
	if len(manifestWorkName) > 63 {
		manifestWorkName = manifestWorkName[:63]
	}

	klog.InfoS("Checking ManifestWork health", "cluster", clusterName, "manifestWorkName", manifestWorkName, "app", e.app.Name, "namespace", e.app.Namespace)

	manifestWork := &ocmworkv1.ManifestWork{}
	err := e.cli.Get(ctx, client.ObjectKey{
		Namespace: clusterName,
		Name:      manifestWorkName,
	}, manifestWork)
	if err != nil {
		if apierrors.IsNotFound(err) {
			klog.InfoS("ManifestWork not found", "cluster", clusterName, "manifestWorkName", manifestWorkName)
			return false, "ManifestWork not found", nil
		}
		klog.ErrorS(err, "Failed to get ManifestWork", "cluster", clusterName, "manifestWorkName", manifestWorkName)
		return false, fmt.Sprintf("failed to get ManifestWork: %v", err), err
	}

	klog.InfoS("ManifestWork found", "cluster", clusterName, "manifestWorkName", manifestWorkName,
		"resourceVersion", manifestWork.ResourceVersion,
		"conditionsCount", len(manifestWork.Status.Conditions))

	// Check ManifestWork conditions
	for _, condition := range manifestWork.Status.Conditions {
		klog.InfoS("ManifestWork condition", "cluster", clusterName, "manifestWorkName", manifestWorkName,
			"type", condition.Type, "status", condition.Status, "reason", condition.Reason, "message", condition.Message)
		if condition.Type == ocmworkv1.WorkApplied && condition.Status == metav1.ConditionTrue {
			klog.InfoS("ManifestWork is healthy (WorkApplied=True)", "cluster", clusterName, "manifestWorkName", manifestWorkName)
			return true, "ManifestWork applied successfully", nil
		}
	}

	klog.InfoS("ManifestWork not ready - WorkApplied condition not True", "cluster", clusterName, "manifestWorkName", manifestWorkName)
	return false, "waiting for ManifestWork to be applied", nil
}

// updateServiceStatus updates the application's service status for OCM deployments
// This is called after deploying ManifestWorks to managed clusters to report component status
func (e *ocmDeployExecutor) updateServiceStatus(components []common.ApplicationComponent, clusters []string, targetNamespace string, clusterHealthMap map[string]clusterDeployResult) {
	// Build a map of existing services for efficient lookup
	existingServices := make(map[string]*common.ApplicationComponentStatus)
	for i := range e.app.Status.Services {
		svc := &e.app.Status.Services[i]
		key := fmt.Sprintf("%s/%s/%s", svc.Name, svc.Namespace, svc.Cluster)
		existingServices[key] = svc
	}

	// For each component on each cluster, create or update service status
	for _, comp := range components {
		for _, cluster := range clusters {
			key := fmt.Sprintf("%s/%s/%s", comp.Name, targetNamespace, cluster)
			clusterResult := clusterHealthMap[cluster]

			status := common.ApplicationComponentStatus{
				Name:      comp.Name,
				Namespace: targetNamespace,
				Cluster:   cluster,
				Healthy:   clusterResult.healthy,
				Message:   clusterResult.message,
			}

			if existing, found := existingServices[key]; found {
				// Update existing service status
				existing.Healthy = status.Healthy
				existing.Message = status.Message
			} else {
				// Add new service status
				e.app.Status.Services = append(e.app.Status.Services, status)
			}
		}
	}

	klog.InfoS("Updated service status for OCM deployment",
		"app", e.app.Name,
		"componentsCount", len(components),
		"clustersCount", len(clusters),
		"totalServicesCount", len(e.app.Status.Services))
}

// cleanupOldManifestWorks removes ManifestWorks that are no longer needed
func (e *ocmDeployExecutor) cleanupOldManifestWorks(ctx context.Context, currentClusters []string) error {
	// List all ManifestWorks with our app labels
	manifestWorkList := &ocmworkv1.ManifestWorkList{}
	err := e.cli.List(ctx, manifestWorkList, client.MatchingLabels{
		oam.LabelAppName:      e.app.Name,
		oam.LabelAppNamespace: e.app.Namespace,
	})
	if err != nil {
		return err
	}

	currentClusterSet := make(map[string]struct{})
	for _, cluster := range currentClusters {
		currentClusterSet[cluster] = struct{}{}
	}

	var wg sync.WaitGroup
	var mu sync.Mutex
	var errs []error

	for _, mw := range manifestWorkList.Items {
		if _, exists := currentClusterSet[mw.Namespace]; !exists {
			wg.Add(1)
			go func(mw ocmworkv1.ManifestWork) {
				defer wg.Done()
				if err := e.cli.Delete(ctx, &mw); err != nil && !apierrors.IsNotFound(err) {
					mu.Lock()
					errs = append(errs, err)
					mu.Unlock()
				}
			}(mw)
		}
	}

	wg.Wait()

	if len(errs) > 0 {
		return errors.Errorf("failed to cleanup old manifest works: %v", errs)
	}

	return nil
}

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
	"testing"

	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clusterv1 "open-cluster-management.io/api/cluster/v1"
	ocmworkv1 "open-cluster-management.io/api/work/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	oamcommon "github.com/oam-dev/kubevela/apis/core.oam.dev/common"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1alpha1"
	"github.com/oam-dev/kubevela/apis/core.oam.dev/v1beta1"
	"github.com/oam-dev/kubevela/pkg/appfile"
	"github.com/oam-dev/kubevela/pkg/utils/common"
)

// testDispatcher creates a dispatcher function for testing that mimics ResourceKeeper behavior
func testDispatcher(cli client.Client) func(ctx context.Context, _ client.Client, cluster, owner string, manifests ...*unstructured.Unstructured) error {
	return func(ctx context.Context, _ client.Client, cluster, owner string, manifests ...*unstructured.Unstructured) error {
		for _, mf := range manifests {
			if mf == nil {
				continue
			}
			// Try to get existing resource
			existing := &unstructured.Unstructured{}
			existing.SetGroupVersionKind(mf.GroupVersionKind())
			err := cli.Get(ctx, client.ObjectKey{Namespace: mf.GetNamespace(), Name: mf.GetName()}, existing)
			if err != nil {
				if apierrors.IsNotFound(err) {
					// Create new resource
					if err := cli.Create(ctx, mf); err != nil {
						return err
					}
					continue
				}
				return err
			}
			// Update existing resource
			mf.SetResourceVersion(existing.GetResourceVersion())
			if err := cli.Update(ctx, mf); err != nil {
				return err
			}
		}
		return nil
	}
}

func TestOCMDeployExecutor(t *testing.T) {
	r := require.New(t)
	ctx := context.Background()

	// Create namespaces for managed clusters (ManifestWork will be created in these namespaces)
	clusterANamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"},
	}
	clusterBNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-b"},
	}

	cli := fake.NewClientBuilder().WithScheme(common.Scheme).WithObjects(
		clusterANamespace,
		clusterBNamespace,
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
	).Build()

	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []oamcommon.ApplicationComponent{
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
					Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a","cluster-b"]}`)},
				},
			},
		},
	}

	af := &appfile.Appfile{
		Name:      "test-app",
		Namespace: "default",
		Policies:  app.Spec.Policies,
	}

	executor := NewOCMDeployExecutor(cli, af, app, DeployOCMParameter{
		Policies:    []string{"ocm-deploy"},
		Parallelism: 5,
	}, testDispatcher(cli))

	// Execute deploy
	healthy, reason, err := executor.Deploy(ctx)
	r.NoError(err)
	// ManifestWork is created but not yet applied by OCM agent, so it should not be healthy yet
	r.False(healthy)
	r.Contains(reason, "not ready")

	// Verify ManifestWorks are created in both cluster namespaces by listing all
	manifestWorkList := &ocmworkv1.ManifestWorkList{}
	err = cli.List(ctx, manifestWorkList)
	r.NoError(err)
	r.Equal(2, len(manifestWorkList.Items))

	// Verify labels on one of the ManifestWorks
	foundClusterA := false
	foundClusterB := false
	for _, mw := range manifestWorkList.Items {
		if mw.Namespace == "cluster-a" {
			foundClusterA = true
			r.Equal("test-app", mw.Labels["app.oam.dev/name"])
			r.Equal("default", mw.Labels["app.oam.dev/namespace"])
		}
		if mw.Namespace == "cluster-b" {
			foundClusterB = true
		}
	}
	r.True(foundClusterA, "ManifestWork for cluster-a should exist")
	r.True(foundClusterB, "ManifestWork for cluster-b should exist")

	// Verify service status is populated for each component on each cluster
	r.Equal(2, len(app.Status.Services), "Should have 2 service entries (1 component * 2 clusters)")
	serviceMap := make(map[string]oamcommon.ApplicationComponentStatus)
	for _, svc := range app.Status.Services {
		key := svc.Name + "/" + svc.Cluster
		serviceMap[key] = svc
	}

	// Check service for cluster-a
	svcA, found := serviceMap["nginx/cluster-a"]
	r.True(found, "Service for nginx on cluster-a should exist")
	r.Equal("nginx", svcA.Name)
	r.Equal("cluster-a", svcA.Cluster)
	r.Equal("default", svcA.Namespace)
	r.False(svcA.Healthy, "Service should not be healthy yet (ManifestWork not applied)")

	// Check service for cluster-b
	svcB, found := serviceMap["nginx/cluster-b"]
	r.True(found, "Service for nginx on cluster-b should exist")
	r.Equal("nginx", svcB.Name)
	r.Equal("cluster-b", svcB.Cluster)
	r.Equal("default", svcB.Namespace)
	r.False(svcB.Healthy, "Service should not be healthy yet (ManifestWork not applied)")
}

func TestOCMDeployExecutorWithEmptyClusters(t *testing.T) {
	r := require.New(t)
	ctx := context.Background()

	cli := fake.NewClientBuilder().WithScheme(common.Scheme).Build()

	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []oamcommon.ApplicationComponent{
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
					Properties: &runtime.RawExtension{Raw: []byte(`{"clusterLabelSelector":{"env":"staging"},"allowEmpty":true}`)},
				},
			},
		},
	}

	af := &appfile.Appfile{
		Name:      "test-app",
		Namespace: "default",
		Policies:  app.Spec.Policies,
	}

	executor := NewOCMDeployExecutor(cli, af, app, DeployOCMParameter{
		Policies:    []string{"ocm-deploy"},
		Parallelism: 5,
	}, testDispatcher(cli))

	// Execute deploy with no matching clusters (allowEmpty = true)
	healthy, _, err := executor.Deploy(ctx)
	r.NoError(err)
	r.True(healthy) // Should be healthy since there's nothing to deploy
}

func TestOCMDeployExecutorWithOverride(t *testing.T) {
	r := require.New(t)
	ctx := context.Background()

	clusterANamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-a"},
	}

	cli := fake.NewClientBuilder().WithScheme(common.Scheme).WithObjects(
		clusterANamespace,
		&clusterv1.ManagedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster-a",
			},
			Spec: clusterv1.ManagedClusterSpec{
				HubAcceptsClient: true,
				ManagedClusterClientConfigs: []clusterv1.ClientConfig{
					{URL: "https://cluster-a.example.com"},
				},
			},
		},
	).Build()

	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []oamcommon.ApplicationComponent{
				{
					Name: "nginx",
					Type: "webservice",
					Properties: &runtime.RawExtension{
						Raw: []byte(`{"image":"nginx:1.19"}`),
					},
				},
			},
			Policies: []v1beta1.AppPolicy{
				{
					Name:       "ocm-deploy",
					Type:       v1alpha1.OCMTopologyPolicyType,
					Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-a"],"namespace":"prod-ns"}`)},
				},
				{
					Name:       "override-prod",
					Type:       v1alpha1.OverridePolicyType,
					Properties: &runtime.RawExtension{Raw: []byte(`{"components":[{"name":"nginx","properties":{"image":"nginx:1.20"}}]}`)},
				},
			},
		},
	}

	af := &appfile.Appfile{
		Name:      "test-app",
		Namespace: "default",
		Policies:  app.Spec.Policies,
	}

	executor := NewOCMDeployExecutor(cli, af, app, DeployOCMParameter{
		Policies:    []string{"ocm-deploy", "override-prod"},
		Parallelism: 5,
	}, testDispatcher(cli))

	_, _, err := executor.Deploy(ctx)
	r.NoError(err)

	// Verify ManifestWork is created by listing all
	manifestWorkList := &ocmworkv1.ManifestWorkList{}
	err = cli.List(ctx, manifestWorkList)
	r.NoError(err)
	r.Equal(1, len(manifestWorkList.Items))

	manifestWork := &manifestWorkList.Items[0]
	r.Equal("cluster-a", manifestWork.Namespace)
	r.Equal(1, len(manifestWork.Spec.Workload.Manifests))

	// The manifest should contain the wrapped application with overridden image
	rawApp := manifestWork.Spec.Workload.Manifests[0].Raw
	r.Contains(string(rawApp), "nginx:1.20")
	// The namespace should be prod-ns
	r.Contains(string(rawApp), "prod-ns")
	// IMPORTANT: Verify the wrapped Application has apiVersion and kind set
	// This is required for ManifestWork to be valid
	r.Contains(string(rawApp), `"apiVersion":"core.oam.dev/v1beta1"`)
	r.Contains(string(rawApp), `"kind":"Application"`)
}

func TestOCMDeployExecutorHealthyStatus(t *testing.T) {
	r := require.New(t)
	ctx := context.Background()

	clusterNamespace := &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{Name: "cluster-test"},
	}

	cli := fake.NewClientBuilder().WithScheme(common.Scheme).WithObjects(
		clusterNamespace,
		&clusterv1.ManagedCluster{
			ObjectMeta: metav1.ObjectMeta{
				Name: "cluster-test",
			},
			Spec: clusterv1.ManagedClusterSpec{
				HubAcceptsClient: true,
			},
		},
	).WithStatusSubresource(&ocmworkv1.ManifestWork{}).Build()

	app := &v1beta1.Application{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-app",
			Namespace: "default",
			UID:       types.UID("test-uid"),
		},
		Spec: v1beta1.ApplicationSpec{
			Components: []oamcommon.ApplicationComponent{
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
					Properties: &runtime.RawExtension{Raw: []byte(`{"clusters":["cluster-test"]}`)},
				},
			},
		},
	}

	af := &appfile.Appfile{
		Name:      "test-app",
		Namespace: "default",
		Policies:  app.Spec.Policies,
	}

	executor := NewOCMDeployExecutor(cli, af, app, DeployOCMParameter{
		Policies:    []string{"ocm-deploy"},
		Parallelism: 5,
	}, testDispatcher(cli))

	// First deploy - ManifestWork is created but not yet applied by OCM agent
	healthy, reason, err := executor.Deploy(ctx)
	r.NoError(err)
	r.False(healthy, "Should not be healthy initially")
	r.Contains(reason, "not ready")

	// Simulate OCM agent setting WorkApplied condition on ManifestWork
	manifestWork := &ocmworkv1.ManifestWork{}
	err = cli.Get(ctx, client.ObjectKey{Namespace: "cluster-test", Name: "test-app-ff814115"}, manifestWork)
	r.NoError(err)
	manifestWork.Status.Conditions = []metav1.Condition{
		{
			Type:   ocmworkv1.WorkApplied,
			Status: metav1.ConditionTrue,
			Reason: "AppliedManifestWorkComplete",
		},
	}
	err = cli.Status().Update(ctx, manifestWork)
	r.NoError(err)

	// Reset app services to simulate a new reconcile
	app.Status.Services = nil

	// Second deploy - should now be healthy since ManifestWork has WorkApplied=True
	healthy, reason, err = executor.Deploy(ctx)
	r.NoError(err)
	r.True(healthy, "Should be healthy when ManifestWork is applied")
	r.Empty(reason, "Reason should be empty when all clusters are healthy")

	// Verify service status is healthy
	r.Equal(1, len(app.Status.Services), "Should have 1 service entry")
	svc := app.Status.Services[0]
	r.Equal("nginx", svc.Name)
	r.Equal("cluster-test", svc.Cluster)
	r.Equal("default", svc.Namespace)
	r.True(svc.Healthy, "Service should be healthy when ManifestWork is applied")
	r.Contains(svc.Message, "successfully", "Message should indicate success")
}

func TestSelectPolicies(t *testing.T) {
	r := require.New(t)

	policies := []v1beta1.AppPolicy{
		{Name: "policy-a", Type: "topology"},
		{Name: "policy-b", Type: "override"},
		{Name: "policy-c", Type: "ocm-topology"},
	}

	// Select all when empty
	selected, err := selectPolicies(policies, []string{})
	r.NoError(err)
	r.Equal(3, len(selected))

	// Select specific policies
	selected, err = selectPolicies(policies, []string{"policy-a", "policy-c"})
	r.NoError(err)
	r.Equal(2, len(selected))
	r.Equal("policy-a", selected[0].Name)
	r.Equal("policy-c", selected[1].Name)

	// Select non-existent policy (no error, just not included)
	selected, err = selectPolicies(policies, []string{"policy-a", "non-existent"})
	r.NoError(err)
	r.Equal(1, len(selected))
}

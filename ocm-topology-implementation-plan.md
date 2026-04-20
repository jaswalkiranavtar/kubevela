# OCM-Topology Policy Implementation Plan

## Overview

Implement a new KubeVela policy called `ocm-topology` that deploys applications to OCM managed clusters by wrapping the entire Application in ManifestWork resources.

**Key Behavior:**
- Deploys to local/hub cluster
- Creates ManifestWork resources containing the wrapped Application
- ManifestWork is placed in namespaces matching managed cluster names
- Only `ocm-topology` and `override` policies are processed; all others remain in the wrapped Application

## Files to Create/Modify

### 1. Policy Type Definition
**File:** `apis/core.oam.dev/v1alpha1/policy_types.go`

Add constant and spec struct:
```go
const OCMTopologyPolicyType = "ocm-topology"

type OCMTopologyPolicySpec struct {
    Placement `json:",inline"`
    Namespace string `json:"namespace,omitempty"`
}
```

### 2. Policy CUE Definition
**Create:** `vela-templates/definitions/internal/policy/ocm-topology.cue`

Define the policy schema with `clusters`, `clusterLabelSelector`, `allowEmpty`, and `namespace` parameters.

### 3. OCM Provider Package
**Create:** `pkg/workflow/providers/ocm/`

Files:
- `ocm.go` - Provider registration and CUE template
- `ocm.cue` - CUE provider definitions
- `deploy.go` - Core deploy logic
- `deploy_test.go` - Unit tests

### 4. OCM Policy Processing
**Create:** `pkg/policy/ocm_topology.go`

Functions:
- `GetManagedClustersFromOCMTopologyPolicies()` - Resolve clusters from policy spec
- `BuildWrappedApplication()` - Create Application with filtered policies

### 5. Workflow Step Definition
**Create:** `vela-templates/definitions/internal/workflowstep/deploy-ocm.cue`

New workflow step that calls `ocm.#DeployOCM` provider.

### 6. Workflow Step Generator
**Modify:** `pkg/workflow/step/generator.go`

Add `OCMDeployWorkflowStepGenerator` to auto-generate `deploy-ocm` steps when `ocm-topology` policies exist.

### 7. Provider Registration
**Modify:** `pkg/workflow/providers/compiler.go`

Register the new OCM provider package.

### 8. Appfile Parser
**Modify:** `pkg/appfile/parser.go`

Add case for `OCMTopologyPolicyType` in `parsePolicies()`.

## Execution Flow

```
Application with ocm-topology + override policies
                    │
                    ▼
Workflow Step Generator detects ocm-topology
Creates deploy-ocm step (combines with override policies)
                    │
                    ▼
deploy-ocm workflow step executes:
  1. Parse ocm-topology policy → get managed cluster names
  2. Parse override policies → get component patches
  3. For each managed cluster:
     a. Build wrapped Application:
        - Apply override patches to components
        - Remove ocm-topology and override policies
        - Keep all other policies, traits, workflow steps
     b. Create ManifestWork in cluster's namespace on hub
                    │
                    ▼
OCM agent picks up ManifestWork → deploys Application on managed cluster
```

## ManifestWork Structure

```yaml
apiVersion: work.open-cluster-management.io/v1
kind: ManifestWork
metadata:
  name: <app-name>-<hash>
  namespace: <managed-cluster-name>  # e.g., "cluster1"
  labels:
    app.oam.dev/name: <app-name>
    app.oam.dev/namespace: <app-namespace>
  ownerReferences:
    - apiVersion: core.oam.dev/v1beta1
      kind: Application
      name: <app-name>
      uid: <app-uid>
spec:
  workload:
    manifests:
      - apiVersion: core.oam.dev/v1beta1
        kind: Application
        metadata:
          name: <app-name>
          namespace: <target-namespace>
        spec:
          components: [...]     # patched by override policies
          policies: [...]       # all EXCEPT ocm-topology and override
          workflow:
            steps: [...]        # original workflow unchanged
```

## Implementation Steps

### Step 1: Add Policy Type (apis/core.oam.dev/v1alpha1/policy_types.go)
- Add `OCMTopologyPolicyType` constant
- Add `OCMTopologyPolicySpec` struct reusing `Placement`

### Step 2: Create Policy CUE Definition
- Create `vela-templates/definitions/internal/policy/ocm-topology.cue`

### Step 3: Create OCM Policy Handler (pkg/policy/ocm_topology.go)
- `GetManagedClustersFromOCMTopologyPolicies()` - handles both explicit clusters and label selectors
- `BuildWrappedApplication()` - constructs the Application to embed in ManifestWork

### Step 4: Create OCM Provider Package
- `pkg/workflow/providers/ocm/ocm.go` - GetTemplate(), GetProviders()
- `pkg/workflow/providers/ocm/ocm.cue` - #DeployOCM definition
- `pkg/workflow/providers/ocm/deploy.go` - DeployOCM() function

### Step 5: Create Workflow Step Definition
- Create `vela-templates/definitions/internal/workflowstep/deploy-ocm.cue`

### Step 6: Update Workflow Step Generator
- Add OCMTopologyPolicyType case in generator
- Create deploy-ocm steps when ocm-topology policies found

### Step 7: Register Provider
- Add OCM provider to compiler.go

### Step 8: Update Appfile Parser
- Add OCMTopologyPolicyType case

### Step 9: Add Tests
- `pkg/policy/ocm_topology_test.go`
- `pkg/workflow/providers/ocm/deploy_test.go`

## Key Code Patterns

### Cluster Resolution (from pkg/policy/topology.go pattern)
```go
func GetManagedClustersFromOCMTopologyPolicies(ctx context.Context, cli client.Client,
    policies []v1beta1.AppPolicy) ([]string, error) {

    for _, policy := range policies {
        if policy.Type == v1alpha1.OCMTopologyPolicyType {
            spec := &v1alpha1.OCMTopologyPolicySpec{}
            utils.StrictUnmarshal(policy.Properties.Raw, spec)

            switch {
            case spec.Clusters != nil:
                // Validate as ManagedCluster resources
                return validateManagedClusters(ctx, cli, spec.Clusters)
            case GetClusterLabelSelectorInTopology(spec) != nil:
                // Query ManagedCluster by labels
                return listManagedClustersByLabels(ctx, cli, selector)
            }
        }
    }
}
```

### ManifestWork Creation
```go
func createManifestWork(ctx context.Context, cli client.Client,
    app *v1beta1.Application, clusterName string,
    wrappedApp *v1beta1.Application) error {

    manifestWork := &ocmworkv1.ManifestWork{
        ObjectMeta: metav1.ObjectMeta{
            Name:      fmt.Sprintf("%s-%s", app.Name, hash),
            Namespace: clusterName,
            OwnerReferences: []metav1.OwnerReference{
                *metav1.NewControllerRef(app, v1beta1.ApplicationKindVersionKind),
            },
        },
        Spec: ocmworkv1.ManifestWorkSpec{
            Workload: ocmworkv1.ManifestsTemplate{
                Manifests: []ocmworkv1.Manifest{{
                    RawExtension: runtime.RawExtension{Object: wrappedApp},
                }},
            },
        },
    }
    return cli.Create(ctx, manifestWork)
}
```

## Verification

### Build KubeVela
```bash
cd /workspaces/kubevela
make manager
```

### Run Unit Tests
```bash
go test ./pkg/policy/... -run TestOCMTopology -v
go test ./pkg/workflow/providers/ocm/... -v
```

### Test in Kind Cluster

1. Create kind cluster:
```bash
kind create cluster --name hub
```

2. Install KubeVela:
```bash
make docker-build
docker tag vela-core:latest oamdev/vela-core:latest
kind load docker-image oamdev/vela-core:latest --name hub
helm install kubevela ./charts/vela-core -n vela-system --create-namespace \
  --set image.tag=latest --set image.pullPolicy=Never
```

3. Create mock ManagedCluster namespace (simulating OCM):
```bash
kubectl create namespace cluster1
```

4. Apply test application:
```yaml
apiVersion: core.oam.dev/v1beta1
kind: Application
metadata:
  name: test-ocm-app
spec:
  components:
    - name: nginx
      type: webservice
      properties:
        image: nginx:latest
  policies:
    - name: ocm-deploy
      type: ocm-topology
      properties:
        clusters:
          - cluster1
```

5. Verify ManifestWork created:
```bash
kubectl get manifestwork -n cluster1
kubectl get manifestwork -n cluster1 -o yaml
```

## Dependencies

- OCM work API already registered: `open-cluster-management.io/api/work/v1` (line 91 in pkg/utils/common/common.go)
- Existing policy patterns in `pkg/policy/topology.go`
- Existing provider patterns in `pkg/workflow/providers/multicluster/`

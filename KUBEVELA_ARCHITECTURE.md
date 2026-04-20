# KubeVela Architecture Explained

This document provides a comprehensive explanation of how KubeVela works at three levels of detail.

---

## Level 1: Super High Level Overview

### What is KubeVela?

KubeVela is a Kubernetes-native application delivery platform that implements the Open Application Model (OAM). It provides a higher-level abstraction for deploying applications, separating the concerns of developers (who define components) from platform engineers (who define traits and policies).

### The Core Flow

```
Application YAML → Parse → Apply Policies → Execute Workflow → Deploy Resources → Track Health
```

### Key Concepts

| Concept | Purpose |
|---------|---------|
| **Components** | Define what to deploy (e.g., webservice, database) |
| **Traits** | Add operational features (e.g., scaling, ingress) |
| **Policies** | Control deployment strategy (e.g., which clusters, overrides) |
| **Workflow** | Define deployment steps and conditions |

### Package Organization (27 packages)

```
/pkg/
├── controller/     ← Main reconciliation logic (the "brain")
├── appfile/        ← Application parsing
├── cue/            ← CUE template engine
├── workflow/       ← Workflow execution
├── policy/         ← Policy system (topology, override, etc.)
├── multicluster/   ← Multi-cluster support
├── resourcekeeper/ ← Resource lifecycle management
├── resourcetracker/← Track deployed resources
├── oam/            ← OAM base types and labels
├── definition/     ← Capability definitions
└── ...             ← Supporting utilities
```

### How It Works (Simplified)

1. User submits an `Application` CR
2. Controller parses components using CUE templates from `ComponentDefinition`
3. Policies modify/replicate components for different environments
4. Workflow executes deployment steps
5. Resources are dispatched to target cluster(s)
6. Health is continuously monitored

---

## Level 2: Detailed Architecture

### 2.1 The Main Controller Loop

The Application controller (`/pkg/controller/core.oam.dev/v1beta1/application/`) orchestrates everything:

```
┌─────────────────────────────────────────────────────────────────┐
│                    APPLICATION RECONCILER                        │
├─────────────────────────────────────────────────────────────────┤
│  1. Parse Application → Appfile                                  │
│  2. Create/Update ApplicationRevision                            │
│  3. Apply Policies (topology, override, replication)             │
│  4. Generate Workflow                                            │
│  5. Execute Workflow (deploy step renders & dispatches)          │
│  6. Evaluate Health                                              │
│  7. Update ResourceTracker                                       │
│  8. Garbage Collect old resources                                │
│  9. Update Application Status                                    │
└─────────────────────────────────────────────────────────────────┘
```

### 2.2 The Parsing System

**`/pkg/appfile/`** converts declarative YAML to an actionable internal representation:

```
Application YAML
     │
     ▼
┌─────────────────┐
│   Appfile       │ ← Internal representation
├─────────────────┤
│ - Components[]  │ ← Parsed from spec + ComponentDefinitions
│ - Traits[]      │ ← Parsed from spec + TraitDefinitions
│ - Policies[]    │ ← Raw policy specs
│ - WorkflowSteps │ ← Workflow definition
│ - Definitions{} │ ← Cached definition lookups
└─────────────────┘
```

**Key Process:**
1. Load `ComponentDefinition` for each component type
2. Evaluate CUE template with user-provided properties
3. Generate Kubernetes manifests (Deployment, Service, etc.)
4. Apply traits as additional manifests or patches

### 2.3 The CUE Engine

**`/pkg/cue/`** provides the templating and validation backbone:

```
ComponentDefinition (CUE template)
     +
Component Properties (user input)
     +
Context (app name, namespace, etc.)
     │
     ▼
┌─────────────────────────────────┐
│   CUE Evaluation Engine         │
│   /pkg/cue/definition/          │
├─────────────────────────────────┤
│ AbstractEngine interface:       │
│ - Complete() → workload + aux   │
│ - Status() → health check       │
│ - GetTemplateContext()          │
└─────────────────────────────────┘
     │
     ▼
Kubernetes Manifests (unstructured)
```

### 2.4 The Policy System

**`/pkg/policy/`** handles cross-cutting deployment concerns:

| Policy Type | Purpose | Package |
|-------------|---------|---------|
| **Topology** | Select target clusters | `topology.go` |
| **Override** | Modify component specs per environment | `override.go` |
| **Replication** | Create multiple component instances | `replication.go` |
| **EnvBinding** | Environment-specific configuration | `envbinding.go` |

**Policy Merging Strategy:**
- Multiple policies of the same type merge recursively
- Arrays concatenate
- Booleans OR together

### 2.5 The Workflow Engine

**`/pkg/workflow/`** executes deployment workflows:

```
Workflow Steps: [step1] → [step2] → [step3]
                   │
                   ▼
┌─────────────────────────────────────────┐
│  Workflow Providers                      │
├─────────────────────────────────────────┤
│  oam/apply     → RenderComponent()      │
│                → ApplyComponent()        │
│  multicluster  → Deploy() to clusters   │
│  query         → Query deployed status  │
│  config        → Manage configurations  │
└─────────────────────────────────────────┘
```

**Deploy Workflow Step Parameters:**
- `policies`: Which policies to apply
- `parallelism`: Concurrent deployments
- `ignoreTerraformComponent`: Skip terraform

### 2.6 Multi-Cluster Support

**`/pkg/multicluster/`** enables deploying to multiple clusters:

```
Control Plane (Hub)
     │
     ├──→ Cluster A (via ClusterGateway)
     ├──→ Cluster B
     └──→ Cluster C

Selection: topology policy specifies cluster labels/names
Dispatch: ResourceKeeper sends manifests to each cluster
Tracking: ResourceTracker records cluster assignments
```

### 2.7 Resource Tracking & Lifecycle

**`/pkg/resourcetracker/`** and **`/pkg/resourcekeeper/`** manage resources:

```
ResourceTracker Types:
┌─────────────────────────────────────────┐
│ Root RT      │ Long-lived, app-scoped   │
│ Current RT   │ Per app-generation       │
│ History RTs  │ Old versions (for GC)    │
│ CompRev RT   │ Component revisions      │
└─────────────────────────────────────────┘

Garbage Collection (3-stage):
1. Mark   → Identify outdated RTs
2. Sweep  → Delete marked RTs
3. Finalize → Handle orphan resources
```

### 2.8 Multi-Stage Trait Dispatch

Traits deploy in three stages:

```
Stage 0: PreDispatch  (e.g., mTLS setup before workload)
    ↓
Stage 1: Default      (workloads + standard traits)
    ↓
Stage 2: PostDispatch (e.g., metrics after workload)
```

---

## Level 3: Deep Technical Details

### 3.1 Core Data Structures

**Appfile (`/pkg/appfile/appfile.go`):**
```go
type Appfile struct {
    Name, Namespace string

    // Parsed results
    ParsedComponents []*Component
    ParsedPolicies   []*Component

    // Cached definitions
    RelatedTraitDefinitions        map[string]*v1beta1.TraitDefinition
    RelatedComponentDefinitions    map[string]*v1beta1.ComponentDefinition
    RelatedWorkflowStepDefinitions map[string]*v1beta1.WorkflowStepDefinition

    // Raw specs
    Policies      []v1beta1.AppPolicy
    Components    []common.ApplicationComponent
    WorkflowSteps []wfTypes.WorkflowStep

    AppRevision *v1beta1.ApplicationRevision
}

type Component struct {
    Name, Type   string
    Params       map[string]interface{}
    Traits       []*Trait
    FullTemplate *Template
    Ctx          process.Context  // CUE processing context
    engine       definition.AbstractEngine
}
```

**OAM Labels (`/pkg/oam/labels.go`):**
```go
const (
    // Core tracking labels
    LabelAppName      = "app.oam.dev/name"
    LabelAppRevision  = "app.oam.dev/revision"
    LabelAppComponent = "app.oam.dev/component"
    LabelAppCluster   = "app.oam.dev/cluster"

    // Resource type markers
    ResourceTypeTrait    = "trait"
    ResourceTypeWorkload = "workload"

    // Trait stage annotations
    AnnotationTraitStage        = "trait.oam.dev/stage"
    TraitStagePreDispatch       = "PreDispatch"
    TraitStagePostDispatch      = "PostDispatch"
)
```

**ResourceTracker Storage (`/pkg/resourcetracker/`):**
```go
// Supports compression for large resource lists
// Feature flags: GzipResourceTracker, ZstdResourceTracker

// Resource reference structure
type ManagedResource struct {
    ClusterObjectReference
    OAMObjectReference
    Data *runtime.RawExtension  // Compressed manifest
}
```

### 3.2 Controller Reconciliation Algorithm

**Main reconciliation (`application_controller.go`):**

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Fetch Application
    app := &v1beta1.Application{}
    if err := r.Get(ctx, req.NamespacedName, app); err != nil {
        return ctrl.Result{}, client.IgnoreNotFound(err)
    }

    // 2. Handle deletion
    if app.DeletionTimestamp != nil {
        return r.handleFinalizer(ctx, app)
    }

    // 3. Parse to Appfile
    appParser := appfile.NewApplicationParser(r.Client, ...)
    appfile, err := appParser.GenerateAppFile(ctx, app)

    // 4. Create ApplicationRevision
    revision := r.createApplicationRevision(ctx, appfile)

    // 5. Build and execute workflow
    workflow := r.generateWorkflow(appfile)
    executor := workflow.NewExecutor(...)
    state, err := executor.ExecuteSteps(ctx, revision, workflow.Steps)

    // 6. Resource management
    resourceKeeper := resourcekeeper.NewResourceKeeper(...)
    resourceKeeper.Dispatch(ctx, manifests, applyOpts...)
    resourceKeeper.GarbageCollect(ctx)

    // 7. Update status
    app.Status = buildStatus(state)
    r.Status().Update(ctx, app)

    return ctrl.Result{RequeueAfter: ...}, nil
}
```

### 3.3 CUE Template Processing Pipeline

**Template evaluation (`/pkg/cue/definition/template.go`):**

```go
type AbstractEngine interface {
    // Complete evaluates template and returns workload + auxiliary resources
    Complete(ctx process.Context, params string, outputPath string) (*types.Output, error)

    // Status evaluates health check template
    Status(ctx process.Context, ref runtime.RawExtension, ...) (string, error)

    // GetTemplateContext returns the CUE context for injection
    GetTemplateContext(ctx process.Context, ...) (map[string]interface{}, error)
}

// Processing pipeline:
// 1. Load template from Definition
// 2. Create CUE runtime with built-in functions
// 3. Inject context: { context: {appName, namespace, ...} }
// 4. Inject parameters: { parameter: {image: "...", ...} }
// 5. Evaluate and extract output field
// 6. Convert to unstructured.Unstructured
```

**CUE Context Structure (`/pkg/cue/process/handle.go`):**
```go
type Context interface {
    SetBase(base model.Instance)
    SetEnvironment(env map[string]string)

    AppName() string
    Namespace() string
    CompName() string

    Output() ([]byte, error)
    GetVar(key string) interface{}
}

// Available in CUE templates as:
// context.name       - application name
// context.namespace  - application namespace
// context.appName    - same as name
// parameter.<key>    - user-provided properties
```

### 3.4 Policy Algorithms

**Topology Policy (`/pkg/policy/topology.go`):**
```go
type TopologyPolicy struct {
    Clusters         []string          // Explicit cluster list
    ClusterSelector  map[string]string // Label-based selection
    Namespace        string            // Override namespace
    ClusterLabelSelector *metav1.LabelSelector
}

// Algorithm:
// 1. If Clusters specified, use directly
// 2. Else if ClusterSelector, query clusters matching labels
// 3. Create placement decisions for each cluster
// 4. Component instances replicate per cluster
```

**Override Policy (`/pkg/policy/override.go`):**
```go
type OverridePolicy struct {
    Components []OverrideComponent
}

type OverrideComponent struct {
    Name       string     // Target component (or "*" for all)
    Type       string     // Or target by type
    Properties Patcher    // Merge/replace properties
    Traits     []Patcher  // Patch traits
}

// Algorithm:
// 1. For each component in app
// 2. Find matching override rules (by name or type)
// 3. Apply JSON merge patch or strategic merge patch
// 4. Result: modified component specs
```

**Replication Policy (`/pkg/policy/replication.go`):**
```go
type ReplicationPolicy struct {
    Keys     []string   // Replica identifiers
    Selector []string   // Components to replicate
}

// Algorithm:
// For each selected component:
//   For each key in Keys:
//     Create new component instance with key suffix
//     Inject key into component context
```

### 3.5 Resource Dispatch Logic

**Dispatch algorithm (`/pkg/resourcekeeper/dispatch.go`):**

```go
func (h *resourceKeeper) Dispatch(ctx context.Context, manifests []*unstructured.Unstructured, ...) error {

    // 1. Admission check (custom validation)
    if err := h.admission(ctx, manifests); err != nil {
        return err
    }

    // 2. Record in ResourceTracker
    if err := h.recordManifests(ctx, manifests); err != nil {
        return err
    }

    // 3. For each manifest, determine target cluster
    for _, manifest := range manifests {
        cluster := getClusterAnnotation(manifest)
        client := h.getClusterClient(cluster)

        // 4. Apply with appropriate strategy
        if h.options.ApplyOnce {
            // Create or skip if exists
            client.Create(ctx, manifest)
        } else {
            // Server-side apply with conflict resolution
            client.Patch(ctx, manifest, client.Apply, ...)
        }
    }

    return nil
}
```

### 3.6 Garbage Collection Algorithm

**GC process (`/pkg/resourcekeeper/gc.go`):**

```go
func (h *resourceKeeper) GarbageCollect(ctx context.Context) (bool, error) {

    // Stage 1: Mark
    // - List all ResourceTrackers for this app
    // - Identify: current, history, obsolete
    // - Mark obsolete for deletion

    for _, rt := range resourceTrackers {
        if rt.Generation < app.Generation - revisionLimit {
            markForDeletion(rt)
        }
    }

    // Stage 2: Sweep
    // - Delete marked ResourceTrackers
    // - Use sharded deletion for efficiency

    for _, rt := range markedForDeletion {
        // Remove finalizer first
        removeFinalizer(rt)
        // Then delete
        client.Delete(ctx, rt)
    }

    // Stage 3: Finalize
    // - Handle orphan resources based on policy:
    //   - OrphanResources: keep as-is
    //   - DeleteOrphanResources: delete them

    for _, orphan := range orphanedResources {
        switch orphanPolicy {
        case DeleteOrphanResources:
            client.Delete(ctx, orphan)
        case OrphanResources:
            removeOAMLabels(orphan)  // Disassociate
        }
    }

    return completed, nil
}
```

### 3.7 Workflow Step Execution

**Deploy step (`/pkg/workflow/providers/multicluster/deploy.go`):**

```go
type deployWorkflowStepExecutor struct {
    app         *v1beta1.Application
    appParser   *appfile.Parser
    appfile     *appfile.Appfile
    placement   *v1alpha1.PlacementDecision
}

func (e *deployWorkflowStepExecutor) Deploy(ctx context.Context) (healthy bool, reason string, err error) {

    // 1. Apply policies to get placements
    placements, err := e.applyPolicies(ctx)

    // 2. For each placement (cluster + components)
    for _, placement := range placements {
        cluster := placement.Cluster

        // 3. Render components for this cluster
        for _, comp := range placement.Components {
            manifests, err := e.renderComponent(ctx, comp, cluster)

            // 4. Dispatch to target cluster
            err = e.resourceKeeper.Dispatch(ctx, manifests, ...)
        }
    }

    // 5. Check health
    for _, placement := range placements {
        for _, comp := range placement.Components {
            healthy, reason := e.checkHealth(ctx, comp)
            if !healthy {
                return false, reason, nil
            }
        }
    }

    return true, "", nil
}
```

### 3.8 Health Check Evaluation

**Health via CUE (`/pkg/cue/definition/health/`):**

```go
// Health check defined in ComponentDefinition:
// status: {
//   customStatus: deployment.status.observedGeneration == deployment.metadata.generation
//   healthPolicy: "deployment.status.readyReplicas == deployment.spec.replicas"
// }

func (e *AbstractEngine) Status(ctx Context, ref runtime.RawExtension) (string, error) {
    // 1. Load observed resource state
    observed := ref.Object

    // 2. Inject into CUE context
    ctx.SetVar("output", observed)

    // 3. Evaluate status template
    result, err := e.cueEngine.Eval(ctx, statusTemplate)

    // 4. Return health status
    // - "healthy" if all conditions pass
    // - "unhealthy" with reason if not
    return result.Status, nil
}
```

### 3.9 Definition Type System

**Definition types (`/pkg/definition/definition.go`):**

```go
const (
    componentDefType    = "component"
    traitDefType        = "trait"
    policyDefType       = "policy"
    workflowStepDefType = "workflow-step"
)

// Definition wraps any *Definition CRD
type Definition struct {
    unstructured.Unstructured
}

func (d *Definition) ToCUE() (*cue.Value, error) {
    // Extract schematic.cue template
    // Parse and return CUE value
}

func (d *Definition) GetType() string {
    // Read annotations to determine type
}
```

### 3.10 Feature Flags

**`/pkg/features/`** controls optional behaviors:

| Feature | Default | Purpose |
|---------|---------|---------|
| `GzipResourceTracker` | true | Compress RT storage |
| `ZstdResourceTracker` | false | Zstd compression |
| `ApplyOnce` | false | Disable drift correction |
| `LegacyResourceTrackerGC` | false | Old GC algorithm |
| `DisableWorkflowContextConfigMapCache` | false | Performance tuning |

---

## Summary Diagram

```
┌──────────────────────────────────────────────────────────────────────────┐
│                           KUBEVELA ARCHITECTURE                           │
├──────────────────────────────────────────────────────────────────────────┤
│                                                                          │
│   ┌─────────────┐      ┌──────────────┐      ┌────────────────┐         │
│   │ Application │─────▶│   Appfile    │─────▶│   Workflow     │         │
│   │    YAML     │      │   Parser     │      │   Executor     │         │
│   └─────────────┘      └──────┬───────┘      └───────┬────────┘         │
│                               │                      │                   │
│          ┌────────────────────┼──────────────────────┘                   │
│          │                    │                                          │
│          ▼                    ▼                                          │
│   ┌──────────────┐    ┌──────────────┐    ┌──────────────┐              │
│   │ CUE Engine   │    │   Policies   │    │ ResourceKeeper│             │
│   │ (templates)  │    │  (topology,  │    │  (dispatch,   │             │
│   │              │    │   override)  │    │    GC)        │             │
│   └──────────────┘    └──────────────┘    └───────┬───────┘             │
│                                                   │                      │
│                    ┌──────────────────────────────┼───────────────┐      │
│                    │                              │               │      │
│                    ▼                              ▼               ▼      │
│            ┌─────────────┐               ┌─────────────┐  ┌───────────┐ │
│            │  Cluster A  │               │  Cluster B  │  │ Cluster C │ │
│            │  (local)    │               │  (remote)   │  │ (remote)  │ │
│            └─────────────┘               └─────────────┘  └───────────┘ │
│                                                                          │
│   Supporting:                                                            │
│   ├── /pkg/oam/           - OAM types & labels                          │
│   ├── /pkg/definition/    - Definition management                       │
│   ├── /pkg/resourcetracker/ - Resource tracking                         │
│   ├── /pkg/multicluster/  - Multi-cluster support                       │
│   ├── /pkg/webhook/       - Admission control                           │
│   └── /pkg/utils/         - Utilities (helm, terraform, etc.)           │
│                                                                          │
└──────────────────────────────────────────────────────────────────────────┘
```

---

## Quick Reference: Package Purposes

| Package | Purpose |
|---------|---------|
| `/pkg/controller/` | Main reconciliation controllers |
| `/pkg/appfile/` | Application parsing and Appfile generation |
| `/pkg/cue/` | CUE template engine and evaluation |
| `/pkg/workflow/` | Workflow execution and providers |
| `/pkg/policy/` | Policy system (topology, override, etc.) |
| `/pkg/multicluster/` | Multi-cluster management |
| `/pkg/resourcekeeper/` | Resource dispatch and garbage collection |
| `/pkg/resourcetracker/` | Track deployed resources |
| `/pkg/oam/` | OAM base types and labels |
| `/pkg/definition/` | Definition CRD management |
| `/pkg/component/` | Component utilities |
| `/pkg/rollout/` | Progressive rollout support |
| `/pkg/webhook/` | Admission webhooks |
| `/pkg/utils/` | Shared utilities |
| `/pkg/features/` | Feature flags |
| `/pkg/addon/` | Addon management |
| `/pkg/auth/` | Authentication |
| `/pkg/builtin/` | Built-in definitions |
| `/pkg/cache/` | Caching utilities |
| `/pkg/cmd/` | CLI commands |
| `/pkg/config/` | Configuration management |
| `/pkg/logging/` | Structured logging |
| `/pkg/monitor/` | Monitoring and metrics |
| `/pkg/registry/` | OCI registry operations |
| `/pkg/schema/` | Schema utilities |
| `/pkg/velaql/` | VelaQL query language |

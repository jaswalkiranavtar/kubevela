# KubeVela Application Processing: End-to-End Flow

This document provides a detailed explanation of how a KubeVela Application is processed from the moment it's submitted to the cluster until resources are deployed and health is monitored.

---

## Table of Contents

1. [Overview](#overview)
2. [Entry Point: The Reconciler](#entry-point-the-reconciler)
3. [Phase 1: Application Parsing](#phase-1-application-parsing)
4. [Phase 2: Revision Management](#phase-2-revision-management)
5. [Phase 3: Policy Application](#phase-3-policy-application)
6. [Phase 4: Workflow Generation](#phase-4-workflow-generation)
7. [Phase 5: Workflow Execution](#phase-5-workflow-execution)
8. [Phase 6: Resource Dispatch](#phase-6-resource-dispatch)
9. [Phase 7: Health Evaluation](#phase-7-health-evaluation)
10. [Phase 8: Garbage Collection](#phase-8-garbage-collection)
11. [Phase 9: Status Update](#phase-9-status-update)
12. [Complete Flow Diagram](#complete-flow-diagram)
13. [Key Data Structures](#key-data-structures)

---

## Overview

When a user submits an Application CR to Kubernetes, KubeVela processes it through a sophisticated reconciliation loop that:

1. **Parses** the Application spec into an internal `Appfile` representation
2. **Creates/updates** an `ApplicationRevision` for versioning
3. **Applies policies** (topology, override, replication, etc.)
4. **Generates** workflow steps from the spec
5. **Executes** the workflow to render and deploy components
6. **Dispatches** resources to target clusters via `ResourceKeeper`
7. **Evaluates** component and trait health
8. **Garbage collects** old resources and revisions
9. **Updates** the Application status

---

## Entry Point: The Reconciler

**File:** `pkg/controller/core.oam.dev/v1beta1/application/application_controller.go`

The `Reconciler` struct is the main controller that watches Application CRs:

```go
type Reconciler struct {
    client.Client
    Scheme   *runtime.Scheme
    Recorder event.Recorder
    options  // appRevisionLimit, concurrentReconciles, etc.
}
```

### Reconcile Function Entry (Line 106-340)

```go
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
    // 1. Fetch the Application
    app := new(v1beta1.Application)
    if err := r.Get(ctx, client.ObjectKey{Name: req.Name, Namespace: req.Namespace}, app); err != nil {
        return r.result(client.IgnoreNotFound(err)).ret()
    }

    // 2. Skip if paused or doesn't match controller requirements
    if ctrlrec.IsPaused(app) {
        return ctrl.Result{}, nil
    }

    // 3. Create AppHandler (includes ResourceKeeper initialization)
    handler, err := NewAppHandler(logCtx, r, app)

    // 4. Handle finalizers (for deletion)
    endReconcile, result, err := r.handleFinalizers(logCtx, app, handler)

    // 5. Generate Appfile from Application spec
    appFile, err := appParser.GenerateAppFile(logCtx, app)

    // 6. Prepare and apply ApplicationRevision
    handler.PrepareCurrentAppRevision(logCtx, appFile)
    handler.FinalizeAndApplyAppRevision(logCtx)

    // 7. Apply policies
    handler.ApplyPolicies(logCtx, appFile)

    // 8. Generate workflow steps
    workflowInstance, runners, err := handler.GenerateApplicationSteps(logCtx, app, appParser, appFile)

    // 9. Execute workflow
    workflowExecutor := executor.New(workflowInstance)
    workflowState, err := workflowExecutor.ExecuteRunners(authCtx, runners)

    // 10. Handle workflow state (Suspending, Terminated, Failed, Executing, Succeeded)
    switch workflowState {
        // ... state-specific handling
    }

    // 11. Evaluate health status
    isHealthy := evalStatus(logCtx, handler, appFile, appParser)

    // 12. State keep (prevent drift)
    r.stateKeep(logCtx, handler, app)

    // 13. Garbage collection
    handler.resourceKeeper.GarbageCollect(logCtx, opts...)

    // 14. Update status
    return r.gcResourceTrackers(logCtx, handler, phase, true, componentsRemoved)
}
```

---

## Phase 1: Application Parsing

**File:** `pkg/appfile/parser.go`

The `Parser` converts an Application CR into an `Appfile` - the internal representation used throughout processing.

### Parser Structure

```go
type Parser struct {
    client     client.Client
    tmplLoader TemplateLoaderFn  // Loads CUE templates from definitions
}
```

### GenerateAppFile (Line 87-101)

```go
func (p *Parser) GenerateAppFile(ctx context.Context, app *v1beta1.Application) (*Appfile, error) {
    // Check if using PublishVersion (versioned deployment)
    if isLatest, appRev, err := p.isLatestPublishVersion(ctx, app); err != nil {
        return nil, err
    } else if isLatest {
        // Use cached revision
        return p.GenerateAppFileFromRevision(appRev)
    }
    // Parse from live Application spec
    return p.GenerateAppFileFromApp(ctx, app)
}
```

### GenerateAppFileFromApp (Line 104-132)

```go
func (p *Parser) GenerateAppFileFromApp(ctx context.Context, app *v1beta1.Application) (*Appfile, error) {
    appFile := newAppFile(app)

    // 1. Parse all components (load ComponentDefinitions, evaluate CUE templates)
    if err = p.parseComponents(ctx, appFile); err != nil {
        return nil, errors.Wrap(err, "failed to parseComponents")
    }

    // 2. Parse workflow steps (load WorkflowStepDefinitions)
    if err = p.parseWorkflowSteps(ctx, appFile); err != nil {
        return nil, errors.Wrap(err, "failed to parseWorkflowSteps")
    }

    // 3. Parse policies (load PolicyDefinitions)
    if err = p.parsePolicies(ctx, appFile); err != nil {
        return nil, errors.Wrap(err, "failed to parsePolicies")
    }

    // 4. Parse referred objects (ref-objects component type)
    if err = p.parseReferredObjects(ctx, appFile); err != nil {
        return nil, errors.Wrap(err, "failed to parseReferredObjects")
    }

    return appFile, nil
}
```

### Component Parsing (Line 521-536)

For each component in the Application spec:

```go
func (p *Parser) parseComponents(ctx context.Context, af *Appfile) error {
    for _, c := range af.app.Spec.Components {
        // 1. Load ComponentDefinition template
        // 2. Parse component properties
        // 3. Parse all traits for this component
        comp, err := p.parseComponent(ctx, c, af.app.Annotations)
        comps = append(comps, comp)
    }
    af.ParsedComponents = comps
    return nil
}
```

---

## Phase 2: Revision Management

**File:** `pkg/controller/core.oam.dev/v1beta1/application/revision.go`

ApplicationRevisions provide versioning, rollback capability, and immutable snapshots.

### PrepareCurrentAppRevision (Line 85-125)

```go
func (h *AppHandler) PrepareCurrentAppRevision(ctx context.Context, af *appfile.Appfile) error {
    // If already using a revision (PublishVersion), use it directly
    if af.AppRevision != nil {
        h.currentAppRev = af.AppRevision
        return nil
    }

    // Gather revision spec (app + all definitions)
    appRev, appRevisionHash, err := h.gatherRevisionSpec(af)
    h.currentAppRev = appRev
    h.currentRevHash = appRevisionHash

    // Get the latest revision for comparison
    h.getLatestAppRevision(ctx)

    // Determine if this is a new revision
    h.isNewRevision, needGenerateRevision, err = h.currentAppRevIsNew(ctx)

    if h.isNewRevision && needGenerateRevision {
        // Generate new revision name: app-v1, app-v2, etc.
        h.currentAppRev.Name, _ = utils.GetAppNextRevision(h.app)
    }

    return nil
}
```

### gatherRevisionSpec (Line 129-205)

Collects all definitions used by the application into the revision:

```go
func (h *AppHandler) gatherRevisionSpec(af *appfile.Appfile) (*v1beta1.ApplicationRevision, string, error) {
    appRev := &v1beta1.ApplicationRevision{
        Spec: v1beta1.ApplicationRevisionSpec{
            Application:             *copiedApp,
            ComponentDefinitions:    map[string]*v1beta1.ComponentDefinition{},
            TraitDefinitions:        map[string]*v1beta1.TraitDefinition{},
            PolicyDefinitions:       map[string]v1beta1.PolicyDefinition{},
            WorkflowStepDefinitions: map[string]*v1beta1.WorkflowStepDefinition{},
            Policies:                map[string]v1alpha1.Policy{},
        },
    }

    // Collect definitions from parsed components
    for _, comp := range af.ParsedComponents {
        appRev.Spec.ComponentDefinitions[comp.Name] = comp.FullTemplate.ComponentDefinition
        for _, trait := range comp.Traits {
            appRev.Spec.TraitDefinitions[trait.Name] = trait.FullTemplate.TraitDefinition
        }
    }

    // Compute hash for comparison
    appRevisionHash, err := ComputeAppRevisionHash(appRev)
    return appRev, appRevisionHash, nil
}
```

### FinalizeAndApplyAppRevision (Line 443-498)

Creates or updates the ApplicationRevision in the cluster:

```go
func (h *AppHandler) FinalizeAndApplyAppRevision(ctx context.Context) error {
    appRev := h.currentAppRev
    appRev.Namespace = h.app.Namespace

    // Set ownership and labels
    appRev.SetOwnerReferences([]metav1.OwnerReference{{
        APIVersion: v1beta1.SchemeGroupVersion.String(),
        Kind:       v1beta1.ApplicationKind,
        Name:       h.app.Name,
        UID:        h.app.UID,
        Controller: ptr.To(true),
    }})

    // Create or update
    if err := h.Get(ctx, key, gotAppRev); err != nil {
        if apierrors.IsNotFound(err) {
            return h.Create(ctx, appRev)
        }
    }
    return h.Update(ctx, appRev)
}
```

---

## Phase 3: Policy Application

**File:** `pkg/controller/core.oam.dev/v1beta1/application/apply.go` (ApplyPolicies at line 397-420)

Policies that generate manifests are dispatched here:

```go
func (h *AppHandler) ApplyPolicies(ctx context.Context, af *appfile.Appfile) error {
    // Generate policy manifests from parsed policies
    policyManifests, err := af.GeneratePolicyManifests(ctx)

    // Add app labels
    for _, policyManifest := range policyManifests {
        util.AddLabels(policyManifest, map[string]string{
            oam.LabelAppName:      h.app.GetName(),
            oam.LabelAppNamespace: h.app.GetNamespace(),
        })
    }

    // Dispatch to cluster
    return h.Dispatch(ctx, h.Client, "", common.PolicyResourceCreator, policyManifests...)
}
```

### Built-in Policy Types (Handled Internally)

These policies are processed during workflow execution, not dispatched as resources:

| Policy Type | Purpose |
|-------------|---------|
| `garbage-collect` | Control resource cleanup behavior |
| `apply-once` | Skip re-applying unchanged resources |
| `shared-resource` | Mark resources as shared across apps |
| `take-over` | Take ownership of existing resources |
| `read-only` | Read but don't modify resources |
| `topology` | Select target clusters |
| `override` | Modify component/trait specs |
| `replication` | Create multiple component instances |

---

## Phase 4: Workflow Generation

**File:** `pkg/controller/core.oam.dev/v1beta1/application/generator.go`

### GenerateApplicationSteps (Line 70-130)

Converts the workflow spec into executable task runners:

```go
func (h *AppHandler) GenerateApplicationSteps(ctx monitorContext.Context,
    app *v1beta1.Application,
    appParser *appfile.Parser,
    af *appfile.Appfile) (*wfTypes.WorkflowInstance, []wfTypes.TaskRunner, error) {

    // Build process context with app metadata
    pCtx := velaprocess.NewContext(generateContextDataFromApp(app, appRev.Name))

    // Create runtime parameters with callbacks for component operations
    ctxWithRuntimeParams := oamprovidertypes.WithRuntimeParams(ctx.GetContext(), oamprovidertypes.RuntimeParams{
        ComponentApply:       h.applyComponentFunc(appParser, af),      // Render + dispatch
        ComponentRender:      h.renderComponentFunc(appParser, af),     // Render only
        ComponentHealthCheck: h.checkComponentHealth(appParser, af),    // Health check
        App:                  app,
        AppLabels:            appLabels,
        Appfile:              af,
        KubeHandlers: &providertypes.KubeHandlers{
            Apply:  h.Dispatch,    // Resource application
            Delete: h.Delete,      // Resource deletion
        },
    })

    // Generate workflow instance
    instance := generateWorkflowInstance(af, app)

    // Generate task runners from workflow steps
    runners, err := generator.GenerateRunners(ctx, instance, wfTypes.StepGeneratorOptions{
        Compiler:       providers.DefaultCompiler.Get(),
        ProcessCtx:     pCtx,
        TemplateLoader: template.NewWorkflowStepTemplateRevisionLoader(appRev, h.Client.RESTMapper()),
        StepConvertor: map[string]func(step) (step, error){
            // Convert apply-component to builtin-apply-component
            wfTypes.WorkflowStepTypeApplyComponent: func(lstep) (step, error) {
                copierStep.Type = wfTypes.WorkflowStepTypeBuiltinApplyComponent
                return *copierStep, nil
            },
        },
    })

    return instance, runners, nil
}
```

### generateWorkflowInstance (Line 156-190)

Creates the workflow instance with proper state:

```go
func generateWorkflowInstance(af *appfile.Appfile, app *v1beta1.Application) *wfTypes.WorkflowInstance {
    instance := &wfTypes.WorkflowInstance{
        WorkflowMeta: wfTypes.WorkflowMeta{
            Name:      af.Name,
            Namespace: af.Namespace,
            UID:       app.UID,
        },
        Steps: af.WorkflowSteps,
        Mode:  af.WorkflowMode,
    }

    // Copy existing status for resume
    instance.Status = copyWorkflowStatusToInstance(app, af.WorkflowMode)

    // Map app phase to workflow phase
    switch app.Status.Phase {
    case common.ApplicationRunning:
        instance.Status.Phase = workflowv1alpha1.WorkflowStateSucceeded
    case common.ApplicationWorkflowSuspending:
        instance.Status.Phase = workflowv1alpha1.WorkflowStateSuspending
    // ...
    }

    return instance
}
```

---

## Phase 5: Workflow Execution

**File:** Uses external `github.com/kubevela/workflow` package

### Workflow Executor (application_controller.go Line 210-221)

```go
workflowExecutor := executor.New(workflowInstance)
workflowState, err := workflowExecutor.ExecuteRunners(authCtx, runners)
```

### Workflow State Handling (Line 261-300)

```go
switch workflowState {
case workflowv1alpha1.WorkflowStateSuspending:
    // Apply PostDispatch traits if enabled
    applyPostDispatchTraits()
    // Requeue with backoff
    return r.gcResourceTrackers(logCtx, handler, common.ApplicationWorkflowSuspending, false, workflowUpdated)

case workflowv1alpha1.WorkflowStateTerminated:
    r.doWorkflowFinish(logCtx, app, handler, workflowState)
    return r.gcResourceTrackers(logCtx, handler, common.ApplicationWorkflowTerminated, false, workflowUpdated)

case workflowv1alpha1.WorkflowStateFailed:
    r.doWorkflowFinish(logCtx, app, handler, workflowState)
    return r.gcResourceTrackers(logCtx, handler, common.ApplicationWorkflowFailed, false, workflowUpdated)

case workflowv1alpha1.WorkflowStateExecuting:
    // Continue executing, requeue
    return r.result(err).requeue(workflowExecutor.GetBackoffWaitTime()).ret()

case workflowv1alpha1.WorkflowStateSucceeded:
    r.doWorkflowFinish(logCtx, app, handler, workflowState)
    // Continue to health evaluation
}
```

---

## Phase 6: Resource Dispatch

**File:** `pkg/controller/core.oam.dev/v1beta1/application/apply.go`

### applyComponentFunc (Line 306-361)

The core function that renders and deploys a component:

```go
func (h *AppHandler) applyComponentFunc(appParser *appfile.Parser, af *appfile.Appfile) oamprovidertypes.ComponentApply {
    return func(baseCtx context.Context, comp common.ApplicationComponent, patcher *cue.Value, clusterName string, overrideNamespace string) (*unstructured.Unstructured, []*unstructured.Unstructured, bool, error) {

        // 1. Set cluster and namespace context
        ctx := multicluster.ContextWithClusterName(baseCtx, clusterName)
        ctx = contextWithComponentNamespace(ctx, overrideNamespace)

        // 2. Parse component from revision and generate manifest
        wl, manifest, err := h.prepareWorkloadAndManifests(ctx, appParser, comp, patcher, af)

        // 3. Render workload and traits with proper labels/annotations
        readyWorkload, readyTraits, err := renderComponentsAndTraits(manifest, appRev, clusterName, overrideNamespace)

        // 4. Check if a trait manages the workload (skip workload apply)
        checkSkipApplyWorkload(wl)

        // 5. Multi-stage dispatch (if enabled)
        if utilfeature.DefaultMutableFeatureGate.Enabled(features.MultiStageComponentApply) {
            manifestDispatchers, err := h.generateDispatcher(appRev, readyWorkload, readyTraits, overrideNamespace, af.AppAnnotations)
            for _, dispatcher := range manifestDispatchers {
                if isHealth, err := dispatcher.run(ctx, wl, appRev, clusterName); !isHealth || err != nil {
                    return nil, nil, false, err
                }
            }
        } else {
            // 6. Single-stage dispatch
            dispatchResources := readyTraits
            if !wl.SkipApplyWorkload {
                dispatchResources = append([]*unstructured.Unstructured{readyWorkload}, readyTraits...)
            }
            if err := h.Dispatch(ctx, h.Client, clusterName, common.WorkflowResourceCreator, dispatchResources...); err != nil {
                return nil, nil, false, err
            }
        }

        // 7. Collect health status
        _, _, _, isHealth, err = h.collectHealthStatus(ctx, wl, overrideNamespace, false)

        return readyWorkload, readyTraits, isHealth, nil
    }
}
```

### ResourceKeeper Dispatch

**File:** `pkg/resourcekeeper/dispatch.go`

```go
func (h *resourceKeeper) Dispatch(ctx context.Context, manifests []*unstructured.Unstructured, applyOpts []apply.ApplyOption, options ...DispatchOption) error {
    // 0. Admission check (validate resources)
    if err = h.AdmissionCheck(ctx, manifests); err != nil {
        return err
    }

    // 1. Pre-dispatch dry-run (if enabled)
    if utilfeature.DefaultMutableFeatureGate.Enabled(features.PreDispatchDryRun) {
        if err = h.dispatch(ctx, manifests, append([]apply.ApplyOption{apply.DryRunAll()}, opts...)); err != nil {
            return fmt.Errorf("pre-dispatch dryrun failed: %w", err)
        }
    }

    // 2. Record manifests in ResourceTracker
    if err = h.record(ctx, manifests, options...); err != nil {
        return err
    }

    // 3. Apply manifests to cluster(s)
    return h.dispatch(ctx, manifests, opts)
}
```

### Resource Recording in ResourceTracker

```go
func (h *resourceKeeper) record(ctx context.Context, manifests []*unstructured.Unstructured, options ...DispatchOption) error {
    var skipGCManifests []*unstructured.Unstructured
    var rootManifests []*unstructured.Unstructured
    var versionManifests []*unstructured.Unstructured

    // Categorize manifests based on GC strategy
    for _, manifest := range manifests {
        switch {
        case cfg.skipGC:
            skipGCManifests = append(skipGCManifests, manifest)
        case cfg.useRoot:
            rootManifests = append(rootManifests, manifest)
        default:
            versionManifests = append(versionManifests, manifest)
        }
    }

    // Record in root RT (long-lived)
    rt, _ := h.getRootRT(ctx)
    resourcetracker.RecordManifestsInResourceTracker(ctx, h.Client, rt, rootManifests, ...)

    // Record in current RT (versioned)
    rt, _ = h.getCurrentRT(ctx)
    resourcetracker.RecordManifestsInResourceTracker(ctx, h.Client, rt, versionManifests, ...)

    return nil
}
```

---

## Phase 7: Health Evaluation

**File:** `pkg/controller/core.oam.dev/v1beta1/application/application_controller.go` (Line 783-797)

```go
func evalStatus(ctx monitorContext.Context, handler *AppHandler, appFile *appfile.Appfile, appParser *appfile.Parser) bool {
    // Run health check function
    healthCheck := handler.checkComponentHealth(appParser, appFile)

    if !hasHealthCheckPolicy(appFile.ParsedPolicies) {
        // Build component map for efficient lookup
        componentMap := make(map[string]common.ApplicationComponent)
        for _, component := range handler.app.Spec.Components {
            componentMap[component.Name] = component
        }

        // Apply health status to services
        applyComponentHealthToServices(ctx, handler, componentMap, healthCheck)
        handler.app.Status.Services = handler.services
        return isHealthy(handler.services)
    }
    return true
}
```

### collectHealthStatus (apply.go Line 313-371)

```go
func (h *AppHandler) collectHealthStatus(ctx context.Context, comp *appfile.Component, overrideNamespace string, skipWorkload bool, traitFilters ...TraitFilter) (*common.ApplicationComponentStatus, *unstructured.Unstructured, []*unstructured.Unstructured, bool, error) {
    status := common.ApplicationComponentStatus{
        Name:      comp.Name,
        Healthy:   true,
        Namespace: accessor.Namespace(),
        Cluster:   multicluster.ClusterNameInContext(ctx),
    }

    // Check workload health
    if !skipWorkload {
        isHealth, output, outputs, err = h.collectWorkloadHealthStatus(ctx, comp, &status, accessor)
    }

    // Check trait health
    for _, tr := range comp.Traits {
        traitStatus, _outputs, err := h.collectTraitHealthStatus(comp, tr, overrideNamespace)
        isHealth = isHealth && traitStatus.Healthy
        traitStatusList = append(traitStatusList, traitStatus)
    }

    status.Traits = traitStatusList
    h.addServiceStatus(true, status)

    return &status, output, outputs, isHealth, nil
}
```

### collectWorkloadHealthStatus (apply.go Line 262-309)

Evaluates health using CUE status template:

```go
func (h *AppHandler) collectWorkloadHealthStatus(ctx context.Context, comp *appfile.Component, status *common.ApplicationComponentStatus, accessor util.NamespaceAccessor) (bool, *unstructured.Unstructured, []*unstructured.Unstructured, error) {

    if comp.CapabilityCategory == types.TerraformCategory {
        // Special handling for Terraform components
        var configuration terraforv1beta2.Configuration
        h.Client.Get(ctx, key, &configuration)
        setStatus(status, configuration.Status.ObservedGeneration, ...)
    } else {
        // Standard CUE-based health evaluation
        templateContext, err := comp.GetTemplateContext(comp.Ctx, h.Client, accessor)
        statusResult, err := comp.EvalStatus(templateContext)

        if statusResult != nil {
            status.Healthy = statusResult.Healthy
            status.Message = statusResult.Message
            status.Details = statusResult.Details
        }
    }

    return status.Healthy, output, outputs, nil
}
```

---

## Phase 8: Garbage Collection

**File:** `pkg/resourcekeeper/gc.go`

KubeVela uses a three-stage garbage collection process:

### GarbageCollect (Line 112-114)

```go
func (h *resourceKeeper) GarbageCollect(ctx context.Context, options ...GCOption) (finished bool, waiting []v1beta1.ManagedResource, err error) {
    return h.garbageCollect(ctx, h.buildGCConfig(ctx, options...))
}
```

### Three-Stage GC Process

```go
func (h *resourceKeeper) garbageCollect(ctx context.Context, cfg *gcConfig) (finished bool, waiting []v1beta1.ManagedResource, err error) {
    gc := gcHandler{resourceKeeper: h, cfg: cfg}
    gc.Init()

    // Stage 1: MARK
    // Identify ResourceTrackers that should be deleted
    // - rootRT/currentRT: only when app is being deleted
    // - historyRTs: when GC mode is not passive, or all resources are recycled
    if !cfg.disableMark {
        if err = gc.Mark(ctx); err != nil {
            return false, waiting, err
        }
    }

    // Stage 2: SWEEP
    // Check if all resources in marked RTs are recycled
    // If yes, remove finalizer from RT
    if !cfg.disableSweep {
        if finished, waiting, err = gc.Sweep(ctx); err != nil {
            return false, waiting, err
        }
    }

    // Stage 3: FINALIZE
    // Delete resources owned by marked RTs
    // Either delete or orphan based on policy
    if !cfg.disableFinalize {
        if finished, waiting, err = gc.Finalize(ctx); err != nil {
            return false, waiting, err
        }
    }

    return finished, waiting, nil
}
```

### ResourceTracker Types

| Type | Purpose | Lifecycle |
|------|---------|-----------|
| **Root RT** | Long-lived app resources | Lives with app |
| **Current RT** | Current generation resources | Lives with app generation |
| **History RTs** | Previous generation resources | GC'd after retention limit |
| **Component Revision RT** | Component revisions | GC'd with revision limit |

---

## Phase 9: Status Update

**File:** `pkg/controller/core.oam.dev/v1beta1/application/application_controller.go`

### writeStatusByMethod (Line 507-534)

```go
func (r *Reconciler) writeStatusByMethod(ctx context.Context, method method, app *v1beta1.Application, phase common.ApplicationPhase) error {
    // Set phase
    app.Status.Phase = phase
    updateObservedGeneration(app)

    // Skip update if status unchanged
    if oldApp, ok := originalAppFrom(ctx); ok && equality.Semantic.DeepEqual(oldApp.Status, app.Status) {
        return nil
    }

    // Patch or Update based on method
    switch method {
    case patch:
        return r.Status().Patch(ctx, app, client.Merge)
    case update:
        return r.Status().Update(ctx, app)
    }

    return nil
}
```

### Application Phases

| Phase | Description |
|-------|-------------|
| `ApplicationStarting` | Initial phase |
| `ApplicationRendering` | Parsing and rendering |
| `ApplicationPolicyGenerating` | Applying policies |
| `ApplicationRunningWorkflow` | Workflow executing |
| `ApplicationWorkflowSuspending` | Workflow suspended |
| `ApplicationWorkflowTerminated` | Workflow terminated |
| `ApplicationWorkflowFailed` | Workflow failed |
| `ApplicationRunning` | Healthy and running |
| `ApplicationUnhealthy` | Deployed but unhealthy |
| `ApplicationDeleting` | Being deleted |

---

## Complete Flow Diagram

```
┌─────────────────────────────────────────────────────────────────────────────────┐
│                        APPLICATION RECONCILIATION FLOW                          │
├─────────────────────────────────────────────────────────────────────────────────┤
│                                                                                 │
│   User Submits Application CR                                                   │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 1. RECONCILER ENTRY                                                   │    │
│   │    • Fetch Application                                                │    │
│   │    • Check paused/controller requirements                             │    │
│   │    • Create AppHandler with ResourceKeeper                            │    │
│   │    • Handle finalizers (deletion flow)                                │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 2. PARSE APPLICATION                                                  │    │
│   │    • Check PublishVersion for cached revision                         │    │
│   │    • parseComponents() - Load ComponentDefinitions, CUE templates     │    │
│   │    • parseWorkflowSteps() - Load WorkflowStepDefinitions             │    │
│   │    • parsePolicies() - Load PolicyDefinitions                        │    │
│   │    • parseReferredObjects() - Handle ref-objects                     │    │
│   │    → Output: Appfile                                                  │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 3. REVISION MANAGEMENT                                                │    │
│   │    • PrepareCurrentAppRevision()                                      │    │
│   │      - Gather all definitions into revision spec                      │    │
│   │      - Compute revision hash                                          │    │
│   │      - Compare with latest revision                                   │    │
│   │    • FinalizeAndApplyAppRevision()                                    │    │
│   │      - Create/Update ApplicationRevision CR                           │    │
│   │    • UpdateAppLatestRevisionStatus()                                  │    │
│   │    → Output: ApplicationRevision in cluster                           │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 4. APPLY POLICIES                                                     │    │
│   │    • GeneratePolicyManifests() for custom policies                    │    │
│   │    • Dispatch policy resources via ResourceKeeper                     │    │
│   │    • Built-in policies (topology, override, etc.) stored for later    │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 5. GENERATE WORKFLOW                                                  │    │
│   │    • GenerateApplicationSteps()                                       │    │
│   │      - Build process context                                          │    │
│   │      - Register component callbacks (apply, render, healthCheck)      │    │
│   │      - Generate workflow instance                                     │    │
│   │      - Generate task runners from workflow steps                      │    │
│   │    → Output: WorkflowInstance + TaskRunners                           │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 6. EXECUTE WORKFLOW                                                   │    │
│   │    • workflowExecutor.ExecuteRunners()                                │    │
│   │    • Each step calls registered callbacks:                            │    │
│   │      - deploy: Applies policies, dispatches to clusters               │    │
│   │      - apply-component: Renders and applies single component          │    │
│   │      - custom steps: Execute CUE templates                            │    │
│   │    → Output: WorkflowState (Executing/Succeeded/Failed/Suspended)     │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 7. DISPATCH RESOURCES (per component)                                 │    │
│   │    • prepareWorkloadAndManifests()                                    │    │
│   │      - Parse component from revision                                  │    │
│   │      - Evaluate CUE template with parameters                          │    │
│   │      - Generate ComponentManifest                                     │    │
│   │    • renderComponentsAndTraits()                                      │    │
│   │      - Apply labels, annotations, cluster info                        │    │
│   │    • ResourceKeeper.Dispatch()                                        │    │
│   │      - Admission check                                                │    │
│   │      - Pre-dispatch dry-run (optional)                                │    │
│   │      - Record in ResourceTracker                                      │    │
│   │      - Apply to target cluster(s)                                     │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 8. EVALUATE HEALTH                                                    │    │
│   │    • evalStatus() for each component                                  │    │
│   │    • collectHealthStatus()                                            │    │
│   │      - Workload health via CUE status template                        │    │
│   │      - Trait health for each trait                                    │    │
│   │    • Update handler.services with health info                         │    │
│   │    → Output: isHealthy boolean                                        │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 9. STATE KEEP (Drift Prevention)                                      │    │
│   │    • If ApplyOnce feature disabled:                                   │    │
│   │      - Re-apply resources to detect/fix drift                         │    │
│   │      - Compare recorded vs actual state                               │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 10. GARBAGE COLLECTION                                                │    │
│   │     Three-Stage Process:                                              │    │
│   │     • MARK: Identify ResourceTrackers to delete                       │    │
│   │     • SWEEP: Remove finalizers from empty RTs                         │    │
│   │     • FINALIZE: Delete/orphan managed resources                       │    │
│   │     Also handles:                                                     │    │
│   │     • Application revision cleanup (retention limit)                  │    │
│   │     • Component revision cleanup                                      │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│   ┌───────────────────────────────────────────────────────────────────────┐    │
│   │ 11. UPDATE STATUS                                                     │    │
│   │     • Set application phase (Running/Unhealthy/etc.)                  │    │
│   │     • Update workflow status                                          │    │
│   │     • Update services status                                          │    │
│   │     • Update applied resources                                        │    │
│   │     • Patch or Update based on changes                                │    │
│   └───────────────────────────────────────────────────────────────────────┘    │
│           │                                                                     │
│           ▼                                                                     │
│       Return Result                                                             │
│       • Requeue after interval for continuous monitoring                        │
│       • Or requeue immediately if workflow still executing                      │
│                                                                                 │
└─────────────────────────────────────────────────────────────────────────────────┘
```

---

## Key Data Structures

### Appfile (`pkg/appfile/appfile.go`)

```go
type Appfile struct {
    Name, Namespace string

    // Parsed components and policies
    ParsedComponents []*Component
    ParsedPolicies   []*Component

    // Raw specs
    Components    []common.ApplicationComponent
    Policies      []v1beta1.AppPolicy
    WorkflowSteps []wfTypesv1alpha1.WorkflowStep

    // Cached definitions
    RelatedComponentDefinitions    map[string]*v1beta1.ComponentDefinition
    RelatedTraitDefinitions        map[string]*v1beta1.TraitDefinition
    RelatedWorkflowStepDefinitions map[string]*v1beta1.WorkflowStepDefinition

    // Workflow configuration
    WorkflowMode *wfTypesv1alpha1.WorkflowExecuteMode

    // Revision info
    AppRevision     *v1beta1.ApplicationRevision
    AppRevisionName string
    AppRevisionHash string
}
```

### AppHandler (`pkg/controller/.../application/apply.go`)

```go
type AppHandler struct {
    client.Client

    app            *v1beta1.Application
    currentAppRev  *v1beta1.ApplicationRevision
    latestAppRev   *v1beta1.ApplicationRevision
    resourceKeeper resourcekeeper.ResourceKeeper

    isNewRevision  bool
    currentRevHash string

    services         []common.ApplicationComponentStatus
    appliedResources []common.ClusterObjectReference
}
```

### ResourceKeeper Interface (`pkg/resourcekeeper/resourcekeeper.go`)

```go
type ResourceKeeper interface {
    // Dispatch resources to clusters
    Dispatch(context.Context, []*unstructured.Unstructured, []apply.ApplyOption, ...DispatchOption) error

    // Delete resources
    Delete(context.Context, []*unstructured.Unstructured, ...DeleteOption) error

    // Garbage collect old resources
    GarbageCollect(context.Context, ...GCOption) (bool, []v1beta1.ManagedResource, error)

    // Prevent configuration drift
    StateKeep(context.Context) error

    // Check if resources are tracked
    ContainsResources([]*unstructured.Unstructured) bool
}
```

---

## Summary

The KubeVela Application processing flow is a sophisticated pipeline that:

1. **Parses** declarative Application specs into internal representations
2. **Versions** everything via ApplicationRevisions for reproducibility
3. **Applies** policies to modify deployment behavior
4. **Generates** executable workflows from the spec
5. **Executes** workflows that render CUE templates and dispatch resources
6. **Tracks** all resources in ResourceTrackers for lifecycle management
7. **Monitors** health continuously via CUE status templates
8. **Cleans up** old resources through three-stage garbage collection
9. **Updates** status to reflect the current state

This design enables KubeVela to provide a powerful, declarative, multi-cluster application delivery platform with strong guarantees around versioning, rollback, and resource lifecycle management.

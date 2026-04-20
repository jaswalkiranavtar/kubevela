# OCM Multi-Cluster Fan-Out via KubeVela Application-Scoped Policies — End-to-End Guide

This document is a complete, reproducible record of how we built and verified a KubeVela
`ocm-topology` policy that fans a single hub Application out to N OCM-managed spoke
clusters, **without modifying KubeVela core**, using the application-scoped policy
feature introduced in upstream KubeVela PR [#7067](https://github.com/kubevela/kubevela/pull/7067).

It is written so that an engineer — or an LLM agent like Claude — can reproduce the
whole setup end-to-end from a clean devcontainer. Every command, every error we hit,
and every fix is captured below.

---

## Table of Contents

1. [Goal and Scope](#1-goal-and-scope)
2. [Why Not the In-Tree Approach](#2-why-not-the-in-tree-approach)
3. [Design Summary](#3-design-summary)
4. [Prerequisites](#4-prerequisites)
5. [Step-by-Step Setup](#5-step-by-step-setup)
6. [Verification Procedure](#6-verification-procedure)
7. [Errors We Hit and How We Fixed Them](#7-errors-we-hit-and-how-we-fixed-them)
8. [File Reference](#8-file-reference)
9. [Command Cheat Sheet](#9-command-cheat-sheet)
10. [Replay Instructions for Claude](#10-replay-instructions-for-claude)

---

## 1. Goal and Scope

### User story

> As a platform engineer, I want to create a **single** KubeVela Application on a hub
> cluster with an `ocm-topology` policy. That Application should be translated into **N**
> Applications, each wrapped in an OCM `ManifestWork` and dropped in the corresponding
> managed-cluster namespace on the hub. Later, the hub Application should aggregate the
> health of every spoke Application.

### What this guide delivers

- One hub `Application` → `N` `ManifestWork`s on the hub, one per target cluster namespace.
- Each `ManifestWork` contains a cloned **spoke `Application`** that the spoke's KubeVela
  reconciles locally (so traits, workflows, etc. still run on the spoke).
- Per-cluster `properties` / `traits` overrides at the policy level (e.g. different image
  or replica count per cluster).
- Hub-side health aggregation via OCM `feedbackRules` → `ManifestWork.status` →
  ComponentDefinition `customStatus` / `healthPolicy`.

### What this guide explicitly does **not** cover

- OCM `ManagedClusterSet` / `Placement` (cluster selection by label selector). Deferred.
- Native KubeVela `override` policy integration. Deferred — overrides are inline in
  `ocm-topology` instead.
- Production-grade RBAC (we grant the OCM work-agent `cluster-admin` on spokes for test
  convenience).
- TLS certificate rotation, image pull secrets, or networking beyond a single Docker host.

---

## 2. Why Not the In-Tree Approach

A prior attempt ([jaswalkiranavtar/kubevela#1](https://github.com/jaswalkiranavtar/kubevela/pull/1))
built `ocm-topology` as a built-in policy type with ~2,700 lines of Go code in KubeVela
core: a new API type (`OCMTopologyPolicySpec`), a new policy handler, a new workflow-step
generator, skip-gates in the existing generators, a CUE provider package, and wiring in
`parser.go` and `compiler.go`.

Problems with that approach:

- **Maintenance burden**: a forked KubeVela has to track every master change forever.
- **Opinionated**: the delivery mechanism (ManifestWork wrapping) is encoded in Go and
  can't be tweaked without a rebuild.
- **No reuse**: other environments (ArgoCD, Flux) can't benefit from a KubeVela-private
  policy type.

The upstream **Application-Scoped Policies** feature ([PR #7067](https://github.com/kubevela/kubevela/pull/7067),
shipping in `v1.11.0-alpha.x`) gives policies a general-purpose pre-render transform hook
that can rewrite `spec.components`, `spec.policies`, `spec.workflow`, labels, annotations,
and workflow context — in CUE, not Go. That turns the entire `ocm-topology` feature into
**two CUE definitions** with zero code changes to KubeVela.

---

## 3. Design Summary

### The two CUE definitions

**`ocm-spoke-application`** — a `ComponentDefinition` that wraps an arbitrary
Application spec into an OCM `ManifestWork` and wires `feedbackRules` + `customStatus` /
`healthPolicy` so the spoke Application's phase surfaces as the hub component's health.

```
  parameter:
    clusterNamespace: string      # hub-side namespace matching the ManagedCluster name
    appSpec:          {...}       # the full spoke Application CR to deploy
    manifestConfigs:  [...]       # (optional) additional feedbackRules for other resources
  output:
    ManifestWork in namespace=<clusterNamespace>
      workload.manifests = [appSpec]
      manifestConfigs    = [app-feedbackRules, ...caller-provided]
  attributes.status:
    customStatus:  reads .status.resourceStatus.manifests[0].statusFeedback.values
    healthPolicy:  isHealth = (spokePhase == "running")
```

**`ocm-topology`** — a `PolicyDefinition` with `scope: "Application"` that runs before
the Application is rendered. It:

1. Clones `context.appComponents` / `context.appPolicies` / `context.appWorkflow` into a
   `_spokeApp` struct, **stripping itself from `appPolicies`** so the spoke doesn't recurse.
2. For each entry in `parameter.clusters`, builds an effective component list by applying
   any matching `overrides` (full replacement of `properties` and/or `traits`).
3. Emits **one `ocm-spoke-application` component per cluster**, each with the
   per-cluster-customized spoke Application spec.
4. Sets `output.policies: []` so the hub Application carries no business policies once
   transformed.

```
  parameter:
    clusters: [...{
        name: string              # the ManagedCluster name == hub-side namespace
        overrides: *[] | [...{
            component: string     # base component name
            properties?: {...}    # full replacement (not merged)
            traits?: [...{...}]   # full replacement (not merged)
        }]
    }]
```

### Runtime flow

```
hub Application ─┐
  components:    │
    - web        │                               ┌─► MW ocm-my-app-cluster1 (ns cluster1)
    - backend    │   app-scoped policy renders  │     workload.manifests = [ spoke App ]
  policies:      ├──────────────────────────────┤
    - ocm-topology (clusters: [c1, c2])         │     feedback rules: .status.status, etc.
                 │                               └─► MW ocm-my-app-cluster2 (ns cluster2)
                 │                                     workload.manifests = [ spoke App + overrides ]
                 │
                 ▼
  components (after transform):
    - ocm-my-app-cluster1 (type: ocm-spoke-application)
    - ocm-my-app-cluster2 (type: ocm-spoke-application)
  policies: []
  workflow: default (apply-component for each)

              │
              ▼ default deploy workflow on hub applies each k8s-objects/MW component
              │
              ▼ OCM work-agent on each spoke pulls its MW, applies the embedded Application
              │
              ▼ spoke KubeVela reconciles Application → creates Deployments/Services locally
              │
              ▼ spoke writes .status.status = "running" when healthy
              │
              ▼ OCM work-agent writes statusFeedback.values back into MW.status on the hub
              │
              ▼ hub KubeVela re-evaluates customStatus / healthPolicy for each MW component
              │
              ▼ hub Application .status.services[i].healthy = true for each cluster
```

### File layout

```
examples/ocm-topology/
├── definitions/
│   ├── ocm-spoke-application.cue   # ComponentDefinition — ManifestWork wrapper + status feedback
│   └── ocm-topology.cue            # PolicyDefinition (scope: Application) — fan-out transform
├── sample-app.yaml                 # Example hub Application (webservice × 2, 2 clusters, 1 override)
└── SETUP-GUIDE.md                  # This document
```

---

## 4. Prerequisites

### Host / devcontainer

- Linux kernel (this guide uses a devcontainer on an x86_64 host).
- Docker Engine reachable from the devcontainer (either via docker-in-docker or the
  host's `/var/run/docker.sock` bind-mounted).
- The devcontainer user must be in the `docker` group **or** have a way to run docker
  commands with elevated group membership (we used `sg docker -c "..."` throughout).
- Privileged access to at least one kind control-plane container (used to bump kernel
  inotify limits without host-level root).

### CLI tools

| Tool          | Version used       | Install                                                                                                                                 |
| ------------- | ------------------ | --------------------------------------------------------------------------------------------------------------------------------------- |
| `docker`      | 27.3.1             | preinstalled in devcontainer                                                                                                            |
| `kubectl`     | v1.35.3            | preinstalled                                                                                                                            |
| `helm`        | v4.1.4             | preinstalled                                                                                                                            |
| `vela`        | v1.11.0-alpha.3    | preinstalled (must match or exceed the app-scoped policies alpha)                                                                       |
| `kind`        | v0.24.0            | `curl -fsSL -o ~/.local/bin/kind https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-amd64 && chmod +x ~/.local/bin/kind`                    |
| `clusteradm`  | v0.11.0 / bundle v0.16.0 | `curl -fsSL -o /tmp/clusteradm.tar.gz https://github.com/open-cluster-management-io/clusteradm/releases/download/v0.11.0/clusteradm_linux_amd64.tar.gz && tar -xzf /tmp/clusteradm.tar.gz -C /tmp/ && mv /tmp/clusteradm ~/.local/bin/clusteradm && chmod +x ~/.local/bin/clusteradm` |

### Kernel settings (important — see [error 4](#error-4-too-many-open-files-inotify-instance-limit) below)

Three kind clusters + OCM + KubeVela easily exceed the default
`fs.inotify.max_user_instances = 128`. Bump before installing KubeVela.

---

## 5. Step-by-Step Setup

All commands below assume the working directory `/workspaces/codebase/kubevela`.
Every command that needs to reach the Docker daemon is wrapped in
`sg docker -c "..."` because the devcontainer's `node` user isn't in the `docker` group
as an effective primary group (see [error 1](#error-1-docker-permission-denied)).

### 5.1 Install kind and clusteradm

```bash
mkdir -p ~/.local/bin
curl -fsSL -o ~/.local/bin/kind \
  https://kind.sigs.k8s.io/dl/v0.24.0/kind-linux-amd64
chmod +x ~/.local/bin/kind

curl -fsSL -o /tmp/clusteradm.tar.gz \
  https://github.com/open-cluster-management-io/clusteradm/releases/download/v0.11.0/clusteradm_linux_amd64.tar.gz
tar -xzf /tmp/clusteradm.tar.gz -C /tmp/
mv /tmp/clusteradm ~/.local/bin/clusteradm
chmod +x ~/.local/bin/clusteradm

export PATH=~/.local/bin:$PATH
```

### 5.2 Create three kind clusters

```bash
sg docker -c "kind create cluster --name hub      --wait 60s"
sg docker -c "kind create cluster --name cluster1 --wait 60s"
sg docker -c "kind create cluster --name cluster2 --wait 60s"
```

All three land on the Docker bridge network named `kind` automatically.

### 5.3 Attach the devcontainer to the kind network and assemble a writable kubeconfig

The default kind kubeconfigs point at `https://127.0.0.1:<random-port>` — the API server
exposed on the **host's** loopback. Inside the devcontainer those host ports are
unreachable, which breaks `clusteradm init`. Fix by connecting the devcontainer to the
kind network and using the internal kubeconfig form (`https://<cluster>-control-plane:6443`).

```bash
sg docker -c "docker network connect kind codebase"     # 'codebase' is our devcontainer name

mkdir -p ~/.kube-work
sg docker -c "kind get kubeconfig --internal --name hub"      > ~/.kube-work/hub.cfg
sg docker -c "kind get kubeconfig --internal --name cluster1" > ~/.kube-work/cluster1.cfg
sg docker -c "kind get kubeconfig --internal --name cluster2" > ~/.kube-work/cluster2.cfg

KUBECONFIG=~/.kube-work/hub.cfg:~/.kube-work/cluster1.cfg:~/.kube-work/cluster2.cfg \
  kubectl config view --flatten > ~/.kube-work/config
chmod 600 ~/.kube-work/config

export KUBECONFIG=~/.kube-work/config
kubectl config get-contexts -o name
# → kind-cluster1 / kind-cluster2 / kind-hub
```

### 5.4 Bump kernel limits (critical)

```bash
sg docker -c "docker exec -i hub-control-plane \
  sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288"
```

Kind containers run privileged and share the host kernel, so this `sysctl` applies
globally. Omitting this step produces `Error: too many open files` inside the
`vela-core` container and a `CrashLoopBackOff`.

### 5.5 Preload OCM images into every kind cluster

Kind nodes can pull from public registries but networking in a devcontainer-on-container
setup is unreliable. Pulling on the host Docker + side-loading via `kind load
docker-image` is the safe path.

```bash
OCM_IMAGES=(
  "quay.io/open-cluster-management/registration-operator:v0.16.0"
  "quay.io/open-cluster-management/registration:v0.16.0"
  "quay.io/open-cluster-management/work:v0.16.0"
  "quay.io/open-cluster-management/placement:v0.16.0"
  "quay.io/open-cluster-management/addon-manager:v0.16.0"
)
for img in "${OCM_IMAGES[@]}"; do
  sg docker -c "docker pull $img"
done

for cluster in hub cluster1 cluster2; do
  sg docker -c "kind load docker-image ${OCM_IMAGES[*]} --name $cluster"
done
```

### 5.6 Initialize OCM on the hub

```bash
clusteradm init --wait --context kind-hub
```

Expect ~30–90 seconds. The `open-cluster-management` namespace gets the `cluster-manager`
operator (3 replicas), and the operator in turn spawns the hub control plane components
in `open-cluster-management-hub` (registration controller, registration-webhook,
work-webhook, placement, addon-manager). Watch with:

```bash
kubectl --context kind-hub get pods -A | grep -i cluster-management
```

### 5.7 Join cluster1 and cluster2 to the hub

Get a bootstrap token and run `clusteradm join` on each spoke, using the **internal**
hub API URL (`https://hub-control-plane:6443`), not the host-loopback one the CLI prints.

```bash
TOKEN_LINE=$(clusteradm get token --context kind-hub 2>&1 | grep -oP 'token=\K[^ ]+' | head -1)
TOKEN="$TOKEN_LINE"

for c in cluster1 cluster2; do
  clusteradm join \
    --hub-token "$TOKEN" \
    --hub-apiserver https://hub-control-plane:6443 \
    --cluster-name "$c" \
    --context "kind-$c"
done

# Then accept the CSRs on the hub (may need a second try — the klusterlet has to
# pull images and register first, which takes ~20s):
clusteradm accept --clusters cluster1,cluster2 --context kind-hub

kubectl --context kind-hub get managedclusters
# NAME       HUB ACCEPTED   URLS                                  JOINED   AVAILABLE
# cluster1   true           https://cluster1-control-plane:6443   True     True
# cluster2   true           https://cluster2-control-plane:6443   True     True
```

### 5.8 Preload KubeVela images and install v1.11.0-alpha.2

**Do not build from master.** We use the released alpha, which has app-scoped policies:

```bash
VELA_IMAGES=(
  "oamdev/vela-core:v1.11.0-alpha.2"
  "oamdev/cluster-gateway:v1.9.0-alpha.2"
  "oamdev/kube-webhook-certgen:v2.4.1"
)
for img in "${VELA_IMAGES[@]}"; do
  sg docker -c "docker pull $img"
done
for cluster in hub cluster1 cluster2; do
  sg docker -c "kind load docker-image ${VELA_IMAGES[*]} --name $cluster"
done

helm repo add kubevela https://kubevela.github.io/charts 2>/dev/null
helm repo update

# Hub: feature gates ON
helm install kubevela kubevela/vela-core --version 1.11.0-alpha.2 \
  --kube-context kind-hub \
  -n vela-system --create-namespace \
  --set image.pullPolicy=IfNotPresent \
  --set multicluster.clusterGateway.image.pullPolicy=IfNotPresent \
  --set featureGates.enableApplicationScopedPolicies=true \
  --set featureGates.enableGlobalPolicies=true \
  --wait --timeout 5m

# Spokes: default install (no feature gates needed — they run stock KubeVela)
for c in cluster1 cluster2; do
  helm install kubevela kubevela/vela-core --version 1.11.0-alpha.2 \
    --kube-context "kind-$c" \
    -n vela-system --create-namespace \
    --set image.pullPolicy=IfNotPresent \
    --set multicluster.clusterGateway.image.pullPolicy=IfNotPresent \
    --wait --timeout 5m &
done
wait
```

### 5.9 Grant the OCM work-agent permission to manage Applications on each spoke

The OCM work-agent service account (`klusterlet-work-sa` in namespace
`open-cluster-management-agent`) needs to create/update/delete the embedded
`Application` CR. Default OCM install grants only narrow permissions. For a test
environment, bind it to `cluster-admin` on each spoke:

```bash
for c in cluster1 cluster2; do
  kubectl --context "kind-$c" apply -f - <<'EOF'
apiVersion: rbac.authorization.k8s.io/v1
kind: ClusterRoleBinding
metadata:
  name: open-cluster-management:klusterlet-work:admin
roleRef:
  apiGroup: rbac.authorization.k8s.io
  kind: ClusterRole
  name: cluster-admin
subjects:
  - kind: ServiceAccount
    name: klusterlet-work-sa
    namespace: open-cluster-management-agent
EOF
done
```

For production, narrow to exactly the API groups the spoke workload needs — at minimum
`core.oam.dev` and `apps` / `""`.

### 5.10 Apply the two CUE definitions on the hub

```bash
vela def apply examples/ocm-topology/definitions/ocm-spoke-application.cue
vela def apply examples/ocm-topology/definitions/ocm-topology.cue

kubectl --context kind-hub get componentdefinitions,policydefinitions -n vela-system \
  | grep -E "ocm-"
```

### 5.11 Preload example-app images into spokes (so `ErrImagePull` doesn't mask success)

```bash
for img in nginx:1.26 nginx:1.27 nginx:stable; do
  sg docker -c "docker pull $img"
done
for cluster in cluster1 cluster2; do
  sg docker -c "kind load docker-image nginx:1.26 nginx:1.27 nginx:stable --name $cluster"
done
```

> **Do not use `:latest` tags in test apps.** Kubelet sets `imagePullPolicy: Always` for
> `:latest`, bypassing kind's local cache and hitting the network.

### 5.12 Apply the sample Application to the hub

```bash
kubectl --context kind-hub apply -f examples/ocm-topology/sample-app.yaml
```

The sample Application has the annotation `policy.oam.dev/auto-revision: "true"` —
required so policy-rendered spec changes create new `ApplicationRevision`s. Without
it, the `ManifestWork` is pinned to the first render and never picks up edits. See
[error 7](#error-7-policy-rendered-edits-dont-regenerate-the-manifestwork).

---

## 6. Verification Procedure

### 6.1 Hub-side: the transform happened

```bash
kubectl --context kind-hub get application my-app -n default -o wide
# EXPECT eventually:
#   PHASE: running   HEALTHY: true   STATUS: "spoke application phase: running"

kubectl --context kind-hub get manifestwork -A
# EXPECT:
#   cluster1   ocm-my-app-cluster1
#   cluster2   ocm-my-app-cluster2

kubectl --context kind-hub get application my-app -n default \
  -o jsonpath='{range .status.services[*]}name={.name} healthy={.healthy} msg={.message}{"\n"}{end}'
# EXPECT:
#   name=ocm-my-app-cluster1 healthy=true msg=spoke application phase: running
#   name=ocm-my-app-cluster2 healthy=true msg=spoke application phase: running
```

### 6.2 Hub-side: the override applied only to cluster2

```bash
kubectl --context kind-hub get manifestwork ocm-my-app-cluster1 -n cluster1 \
  -o jsonpath='{.spec.workload.manifests[0].spec.components[?(@.name=="web")].properties.image}{"\n"}'
# nginx:1.26  (base)

kubectl --context kind-hub get manifestwork ocm-my-app-cluster2 -n cluster2 \
  -o jsonpath='{.spec.workload.manifests[0].spec.components[?(@.name=="web")].properties.image}{"\n"}'
# nginx:1.27  (overridden)

kubectl --context kind-hub get manifestwork ocm-my-app-cluster2 -n cluster2 \
  -o jsonpath='{.spec.workload.manifests[0].spec.components[?(@.name=="backend")].traits}{"\n"}'
# [{"properties":{"replicas":3},"type":"scaler"}]   (override-only trait)
```

### 6.3 Spoke-side: Applications reconciled and pods are healthy

```bash
for c in cluster1 cluster2; do
  echo "=== kind-$c ==="
  kubectl --context "kind-$c" get application,deploy,pod -n default
done
# EXPECT:
#   kind-cluster1  deploy/web 1/1  deploy/backend 1/1  (base)
#   kind-cluster2  deploy/web 1/1  deploy/backend 3/3  (override scaler replicas: 3)
```

### 6.4 Status feedback round-trips from spoke → hub

```bash
kubectl --context kind-hub get manifestwork ocm-my-app-cluster1 -n cluster1 \
  -o jsonpath='{.status.resourceStatus.manifests[0].statusFeedback.values}' \
  | python3 -m json.tool
# EXPECT:
#   [{"fieldValue":{"string":"running","type":"String"},"name":"status"}, ...]
```

---

## 7. Errors We Hit and How We Fixed Them

Each entry below is a real error we hit during the original run. When you replay, you
will likely hit some or all of them again depending on your environment.

### Error 1: `docker permission denied`

```
docker ps
permission denied while trying to connect to the docker API at unix:///var/run/docker-host.sock
```

**Root cause.** The devcontainer `node` user has `docker` listed in supplementary groups
via `/etc/group` (`getent group docker` shows it), but the running shell's effective
supplementary groups don't include it (`id` shows only `1000(node),1001`).

**Fix.** Wrap every Docker-touching command with `sg docker -c "..."`. This re-executes
the command with the `docker` group as an additional group.

```bash
sg docker -c "docker ps"
```

Do **not** try to `sudo`; the devcontainer's sudoers requires a password.

### Error 2: `clusteradm init` fails with connection refused on 127.0.0.1:<port>

```
Preflight check: cluster-info check Failed
  [ERROR cluster-info check]: Get "https://127.0.0.1:43867/api/v1/namespaces/kube-public/configmaps/cluster-info":
    dial tcp 127.0.0.1:43867: connect: connection refused
```

**Root cause.** Kind writes a kubeconfig with `server: https://127.0.0.1:<random-port>`,
the API server's **host** port mapping. The devcontainer has its own network namespace;
that host port isn't reachable from here.

**Fix.** Attach the devcontainer to the `kind` Docker network and use the *internal*
form of the kubeconfig:

```bash
sg docker -c "docker network connect kind codebase"
sg docker -c "kind get kubeconfig --internal --name hub" > ~/.kube-work/hub.cfg
# → server: https://hub-control-plane:6443
```

### Error 3: `ImagePullBackOff` on `cluster-manager-*` pods

```
kubectl get pods -A
→ open-cluster-management   cluster-manager-... 0/1   ImagePullBackOff
```

**Root cause.** kind nodes can't reach `quay.io` reliably in a nested-container
networking setup.

**Fix.** Pull on the host Docker, `kind load docker-image` into each cluster, then
delete the BackOff pods so they restart with the local image. See
[§5.5](#55-preload-ocm-images-into-every-kind-cluster).

### Error 4: `too many open files` (inotify instance limit)

```
Error: too many open files
I0420 ... "Failed to run manager" err="too many open files"
```

**Root cause.** Default `fs.inotify.max_user_instances = 128`. Three kind clusters +
OCM + KubeVela exceed it; `controller-runtime` can't create watches and vela-core
crashes immediately.

**Fix.** Bump from inside a privileged kind container (affects the shared kernel):

```bash
sg docker -c "docker exec -i hub-control-plane \
  sysctl -w fs.inotify.max_user_instances=8192 fs.inotify.max_user_watches=524288"
```

Then delete the crashing `vela-core` pod so it restarts cleanly:

```bash
kubectl delete pods -n vela-system -l app.kubernetes.io/name=vela-core
```

### Error 5: `managedclusters ... not found, no csr is approved yet`

```
clusteradm accept --clusters cluster1,cluster2 ...
→ Error: [fail to get managedcluster cluster1: ... not found, no csr is approved yet ...]
```

**Root cause.** The klusterlet registration agent on each spoke takes ~20s after
`clusteradm join` to pull images and create its bootstrap CSR. Accepting before the
CSR exists errors out.

**Fix.** Wait until the klusterlet is `Running` on each spoke, then rerun `accept`:

```bash
for c in cluster1 cluster2; do
  kubectl --context "kind-$c" wait --for=condition=Ready pods \
    -n open-cluster-management-agent -l app=klusterlet-registration-agent --timeout=120s
done
clusteradm accept --clusters cluster1,cluster2 --context kind-hub
```

### Error 6: RBAC — OCM work-agent can't manage Applications on the spoke

```
.status.resourceStatus.manifests[0].conditions:
  - reason: AppliedManifestFailed
    message: applications.core.oam.dev "my-app" is forbidden:
             User "system:serviceaccount:open-cluster-management-agent:klusterlet-work-sa"
             cannot get resource "applications" in API group "core.oam.dev" ...
```

**Root cause.** OCM's default spoke-side RBAC does not grant access to arbitrary CRDs
including KubeVela's `Application`.

**Fix.** Bind `klusterlet-work-sa` to `cluster-admin` on each spoke
(test-convenience; tighten for production). See [§5.9](#59-grant-the-ocm-work-agent-permission-to-manage-applications-on-each-spoke).

### Error 7: policy-rendered edits don't regenerate the ManifestWork

After editing the hub `Application.spec.components[].properties.image` and re-applying,
the `ManifestWork.spec.workload.manifests[0]` still has the **old** spec.

**Root cause.** With application-scoped policies, the controller renders a new spec
**in-memory** on every reconcile, but it only creates a new `ApplicationRevision`
(which is what the `ManifestWork` tracks) when the rendered spec actually changes
**and** the Application opts in via annotation:

```go
// pkg/controller/.../application_policies.go
func shouldAutoCreateRevision(app *v1beta1.Application) bool {
    return app.Annotations[oam.AnnotationAutoRevision] == annotationValueTrue
}
```

**Fix.** Add the annotation to the hub Application:

```yaml
metadata:
  annotations:
    policy.oam.dev/auto-revision: "true"
```

### Error 8: `customStatus` / `healthPolicy` always reports `"unknown"`

Spoke apps are healthy, OCM feedback values show `name: "status", string: "running"`,
but the hub Application's `.status.services[i].healthy` stays `false` with
`msg: "spoke application phase: unknown"`.

**Root cause #1.** Field-name typo. OCM's API uses `StatusFeedback` (singular).
Brian Kane's original draft used `statusFeedbacks` (plural). CUE's optional-field
lookup returns bottom (`_|_`), which silently coerces to the default.

**Root cause #2.** The compact Brian-style pattern:

```cue
_feedbacks: *[] | [...] & {
    if len(_manifests) > 0 {
        _manifests[0].statusFeedback.values
    }
}
```

is syntactically unusual and relies on embedding semantics that don't reliably flow
a list through the default/union unification. In practice, `_feedbacks` stayed `[]`.

**Fix.** Use the correct field name and rewrite the guards as explicit nested `if`
checks that assign into `_feedbacks` only when every hop exists:

```cue
_feedbacks: *[] | [...{...}]
if context.output.status != _|_
if context.output.status.resourceStatus != _|_
if context.output.status.resourceStatus.manifests != _|_
if len(context.output.status.resourceStatus.manifests) > 0
if context.output.status.resourceStatus.manifests[0].statusFeedback != _|_
if context.output.status.resourceStatus.manifests[0].statusFeedback.values != _|_ {
    _feedbacks: context.output.status.resourceStatus.manifests[0].statusFeedback.values
}
```

See the current (fixed) definition at
`examples/ocm-topology/definitions/ocm-spoke-application.cue`.

### Error 9: `ComponentDefinition` update doesn't take effect on an existing Application

After `vela def apply` with a fixed `customStatus`, the hub Application still evaluated
the old version.

**Root cause.** An `ApplicationRevision` pins the `DefinitionRevision` it used.
Editing the `ComponentDefinition` creates a new `DefinitionRevision` (e.g. `-v2`),
but the existing `ApplicationRevision` sticks with `-v1` forever. Only a new
`ApplicationRevision` picks up the new definition.

**Fix.** Delete and re-create the hub Application (or use a strategy to force a new
`ApplicationRevision`). In this testbed:

```bash
kubectl --context kind-hub delete application my-app -n default --wait=true
kubectl --context kind-hub apply -f examples/ocm-topology/sample-app.yaml
```

### Error 10: `docker pull img1 img2` fails

```
docker pull nginx:latest nginx:1.27
"docker pull" requires exactly 1 argument.
```

**Root cause.** `docker pull` takes one image per invocation. `kind load docker-image`
accepts multiple, but `docker pull` does not.

**Fix.** Loop:

```bash
for img in nginx:1.26 nginx:1.27; do sg docker -c "docker pull $img"; done
```

---

## 8. File Reference

### 8.1 `examples/ocm-topology/definitions/ocm-spoke-application.cue`

```cue
"ocm-spoke-application": {
	type: "component"
	annotations: {}
	labels: {}
	description: "Deploys a KubeVela Application to an OCM-managed spoke cluster by wrapping it in a ManifestWork."
	attributes: {
		workload: type: "autodetects.core.oam.dev"
		status: {
			customStatus: #"""
				_feedbacks: *[] | [...{...}]
				if context.output.status != _|_
				if context.output.status.resourceStatus != _|_
				if context.output.status.resourceStatus.manifests != _|_
				if len(context.output.status.resourceStatus.manifests) > 0
				if context.output.status.resourceStatus.manifests[0].statusFeedback != _|_
				if context.output.status.resourceStatus.manifests[0].statusFeedback.values != _|_ {
					_feedbacks: context.output.status.resourceStatus.manifests[0].statusFeedback.values
				}
				_spokePhase: *"unknown" | string
				for v in _feedbacks if v.name == "status" {
					_spokePhase: v.fieldValue.string
				}
				message: "spoke application phase: \(_spokePhase)"
				"""#
			healthPolicy: #"""
				_feedbacks: *[] | [...{...}]
				if context.output.status != _|_
				if context.output.status.resourceStatus != _|_
				if context.output.status.resourceStatus.manifests != _|_
				if len(context.output.status.resourceStatus.manifests) > 0
				if context.output.status.resourceStatus.manifests[0].statusFeedback != _|_
				if context.output.status.resourceStatus.manifests[0].statusFeedback.values != _|_ {
					_feedbacks: context.output.status.resourceStatus.manifests[0].statusFeedback.values
				}
				_spokePhase: *"" | string
				for v in _feedbacks if v.name == "status" {
					_spokePhase: v.fieldValue.string
				}
				isHealth: _spokePhase == "running"
				"""#
		}
	}
}

template: {
	_appManifestConfig: {
		resourceIdentifier: {
			group:     "core.oam.dev"
			resource:  "applications"
			namespace: parameter.appSpec.metadata.namespace
			name:      parameter.appSpec.metadata.name
		}
		feedbackRules: [
			{type: "WellKnownStatus"},
			{
				type: "JSONPaths"
				jsonPaths: [
					{name: "status",             path: ".status.status"},
					{name: "observedGeneration", path: ".status.observedGeneration"},
					{name: "workflowPhase",      path: ".status.workflow.phase"},
					{name: "workflowFinished",   path: ".status.workflow.finished"},
				]
			},
		]
	}

	output: {
		apiVersion: "work.open-cluster-management.io/v1"
		kind:       "ManifestWork"
		metadata: {
			name:      context.name
			namespace: parameter.clusterNamespace
		}
		spec: {
			workload: manifests: [parameter.appSpec]
			manifestConfigs: [_appManifestConfig] + parameter.manifestConfigs
		}
	}

	parameter: {
		clusterNamespace: string
		appSpec: {
			apiVersion: string
			kind:       string
			metadata: {
				name:      string
				namespace: string
				...
			}
			spec: {}
			...
		}
		manifestConfigs: *[] | [...{
			resourceIdentifier: {
				group:     string
				resource:  string
				namespace: string
				name:      string
			}
			feedbackRules: [...{type: string, ...}]
		}]
	}
}
```

### 8.2 `examples/ocm-topology/definitions/ocm-topology.cue`

```cue
"ocm-topology": {
	annotations: {}
	labels: {}
	description: "Fan out a KubeVela Application to N OCM managed clusters, wrapping the Application in a ManifestWork per cluster with optional per-cluster component overrides."
	attributes: scope: "Application"
	type: "policy"
}

template: {
	_spokeAppName: context.appName

	_spokePolicies: [for p in context.appPolicies if p.name != context.policyName {p}] & [...{}]

	_componentsFor: {
		for c in parameter.clusters {
			"\(c.name)": [
				for comp in context.appComponents {
					let _matches = [for o in c.overrides if o.component == comp.name {o}]
					if len(_matches) == 0 {comp}
					if len(_matches) > 0 {
						{
							name: comp.name
							type: comp.type
							if _matches[0].properties != _|_ { properties: _matches[0].properties }
							if _matches[0].properties == _|_ if comp.properties != _|_ { properties: comp.properties }
							if _matches[0].traits != _|_ { traits: _matches[0].traits }
							if _matches[0].traits == _|_ if comp.traits != _|_ { traits: comp.traits }
							if comp.dependsOn != _|_        { dependsOn: comp.dependsOn }
							if comp.inputs != _|_           { inputs: comp.inputs }
							if comp.outputs != _|_          { outputs: comp.outputs }
							if comp.externalRevision != _|_ { externalRevision: comp.externalRevision }
						}
					}
				},
			]
		}
	}

	output: {
		labels: "app.oam.dev/managed-by": "ocm"
		components: [
			for c in parameter.clusters {
				name: "ocm-\(_spokeAppName)-\(c.name)"
				type: "ocm-spoke-application"
				properties: {
					clusterNamespace: c.name
					appSpec: {
						apiVersion: "core.oam.dev/v1beta1"
						kind:       "Application"
						metadata: { name: _spokeAppName, namespace: context.namespace }
						spec: {
							components: _componentsFor[c.name]
							if len(_spokePolicies) > 0 { policies: _spokePolicies }
							if context.appWorkflow != null { workflow: context.appWorkflow }
						}
					}
				}
			},
		]
		policies: []
	}

	parameter: {
		clusters: [...{
			name: string
			overrides: *[] | [...{
				component: string
				properties?: {...}
				traits?: [...{
					type: string
					properties?: {...}
				}]
			}]
		}]
	}
}
```

### 8.3 `examples/ocm-topology/sample-app.yaml`

```yaml
apiVersion: core.oam.dev/v1beta1
kind: Application
metadata:
  name: my-app
  namespace: default
  annotations:
    # Required for app-scoped policies: trigger new ApplicationRevisions when
    # the policy-rendered spec changes (e.g. cluster list, overrides).
    policy.oam.dev/auto-revision: "true"
spec:
  components:
    - name: web
      type: webservice
      properties:
        image: nginx:1.26
        port: 80
    - name: backend
      type: webservice
      properties:
        image: nginx:stable
        port: 8080
  policies:
    - name: fan-out
      type: ocm-topology
      properties:
        clusters:
          # cluster1 uses the base component specs as-is
          - name: cluster1
          # cluster2 overrides web's image and adds a scaler trait to backend
          - name: cluster2
            overrides:
              - component: web
                properties:
                  image: nginx:1.27
                  port: 80
              - component: backend
                traits:
                  - type: scaler
                    properties:
                      replicas: 3
```

---

## 9. Command Cheat Sheet

Export once per shell:

```bash
export PATH=~/.local/bin:$PATH
export KUBECONFIG=~/.kube-work/config
```

| Action                                          | Command                                                                                      |
| ----------------------------------------------- | -------------------------------------------------------------------------------------------- |
| List contexts                                   | `kubectl config get-contexts -o name`                                                        |
| Apply/re-apply the definitions                  | `vela def apply examples/ocm-topology/definitions/ocm-spoke-application.cue` <br> `vela def apply examples/ocm-topology/definitions/ocm-topology.cue` |
| Apply the sample app                            | `kubectl --context kind-hub apply -f examples/ocm-topology/sample-app.yaml`                   |
| Force new AppRevision (after editing a def)     | `kubectl --context kind-hub delete application my-app -n default --wait=true` <br> `kubectl --context kind-hub apply -f examples/ocm-topology/sample-app.yaml` |
| Hub app status                                  | `kubectl --context kind-hub get application my-app -n default -o wide`                        |
| Per-cluster health                              | `kubectl --context kind-hub get application my-app -n default -o jsonpath='{range .status.services[*]}{.name} healthy={.healthy} msg={.message}{"\n"}{end}'` |
| Hub ManifestWorks                               | `kubectl --context kind-hub get manifestwork -A`                                              |
| Inspect a MW's wrapped Application              | `kubectl --context kind-hub get manifestwork ocm-my-app-cluster1 -n cluster1 -o jsonpath='{.spec.workload.manifests[0]}' \| python3 -m json.tool` |
| MW status feedback values                       | `kubectl --context kind-hub get manifestwork ocm-my-app-cluster1 -n cluster1 -o jsonpath='{.status.resourceStatus.manifests[0].statusFeedback.values}' \| python3 -m json.tool` |
| Spoke pods                                      | `kubectl --context kind-cluster1 get app,deploy,pod -n default`                                |

---

## 10. Replay Instructions for Claude

To reproduce this whole flow with a fresh Claude agent session, paste the following
instruction into the first message:

> Please follow the guide at `examples/ocm-topology/SETUP-GUIDE.md` end to end to build
> and verify a multi-cluster OCM fan-out KubeVela Application using application-scoped
> policies.
>
> Execute every command in sections **5.1 through 5.12**, then run all checks in
> **section 6** to verify success. Use the file contents in **section 8** as the source
> of truth for the two CUE definitions and the sample Application.
>
> If any of the ten errors listed in **section 7** appear, apply the matching fix from
> that same section before continuing. Do not improvise fixes the guide doesn't list —
> if you hit a new error, stop and ask before proceeding.
>
> Constraints:
> - Do not build KubeVela from master. Use the released `v1.11.0-alpha.2` image.
> - Do not modify any file under `pkg/`, `apis/`, `charts/`, or `cmd/` — this solution
>   is strictly external CUE + YAML.
> - Do not teardown the clusters after verification unless explicitly asked.
> - Wrap every Docker-touching command in `sg docker -c "..."` because the devcontainer's
>   active groups don't include `docker` by default.
> - Use internal kind kubeconfigs (`kind get kubeconfig --internal`) because the
>   devcontainer cannot reach host-port-mapped API servers.

Claude should finish with:

- `kubectl --context kind-hub get application my-app -n default -o wide` showing
  `PHASE: running, HEALTHY: true`.
- Both `ManifestWork`s present on the hub.
- Both spoke deployments healthy, with `cluster2/backend` at **3 replicas** (proof the
  override fired).
- `.status.services[]` with `healthy=true` for both entries (proof status feedback
  round-tripped through OCM).

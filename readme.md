# QuotaScale Controller

QuotaScale Controller is a Kubernetes controller that automatically adjusts namespace `ResourceQuota` for CPU and memory. It watches a custom resource named `QuotaAutoscaler`, tracks the target namespace quota usage, reacts immediately to quota-denied workload creation events, and updates the managed `ResourceQuota`.

When node scaling is enabled, the controller also coordinates a second control loop for scaling-dedicated nodes through GitOps.

For the end-to-end demo manifests and Locust traffic replay setup, see [example/test/README.md](example/test/README.md).

Container images are published to `ghcr.io/ssu-dcn/quotascale-controller`. Every pushed commit receives an immutable `sha-<short-sha>` tag; pushes to `main` also update `latest`.

## What this project does

- Automatically scales `ResourceQuota` `requests.cpu`, `limits.cpu`, `requests.memory`, and `limits.memory`
- Uses percentage-based policies for scale-up and scale-down
- Supports per-namespace minimum and maximum CPU and memory bounds
- Reacts immediately when workload creation is denied because quota is exhausted
- Optionally triggers node scale-out / scale-in through a Git-managed `MachineDeployment`

## Main components

### Quota controller

The quota controller is the main loop for namespace quota management.

It watches:

- `QuotaAutoscaler`
- `ResourceQuota`
- Kubernetes `Event` objects related to quota-denied workload creation

Its responsibilities are:

- match each `QuotaAutoscaler` to its target `ResourceQuota`
- calculate desired CPU and memory quota from current usage and policy
- enforce `min.cpu`, `max.cpu`, `min.memory`, `max.memory`
- call the resize worker when quota should change
- request node scale-out first when desired quota does not fit current worker-node capacity
- ignore stale quota-denied events that happened before the current controller process started

### Resize worker

The resize worker serializes quota updates per namespace and rate-limits repeated resize operations.

By default, the stub implementation patches the target `ResourceQuota` directly. The implementation lives in `internal/resize/api.go`.

### Node scaling controller

The node scaling controller is optional and is enabled with `--enable-node-scaling=true`.

Its responsibilities are:

- maintain a `NodeScalingInventory` that reflects observed scaling nodes plus desired `MachineDeployment` replicas
- keep a configurable number of warm spare scaling nodes prepared
- activate one or more prepared spare nodes when quota scale-out needs more cluster capacity
- update a Git-managed `MachineDeployment` replica count
- evaluate automatic scale-in after managed quota totals fit without scaling-tainted nodes for a configured delay
- reserve multiple scale-in candidates concurrently and reuse lower-order candidates first when a new scale-out arrives

## Custom resources

### 1. QuotaAutoscaler

`QuotaAutoscaler` is a namespaced CRD in the `dcn.ssu.ac.kr/v1` API group.

Purpose:

- declare which `ResourceQuota` should be managed
- define scale-up and scale-down thresholds
- define the target utilization after scaling
- define CPU and memory bounds

Key spec fields:

- `resourceQuota`: optional existing target `ResourceQuota` name in the same namespace; when omitted, the controller creates `resource-quota`
- `min.cpu`
- `max.cpu`
- `min.memory`
- `max.memory`
- `behavior.scaleUp.policies[]`
- `behavior.scaleDown.policies[]`

Each policy supports:

- `method`: `cpu` or `memory`
- `value`: threshold percentage
- `targetUtilization`: desired utilization percentage after scaling

If `spec.resourceQuota` is omitted, the controller creates a `ResourceQuota` named `resource-quota` in the `QuotaAutoscaler` namespace. Its initial `requests.cpu` and `limits.cpu` hard values come from `min.cpu`; its initial `requests.memory` and `limits.memory` hard values come from `min.memory`. The created quota has the `QuotaAutoscaler` as its controller owner, and the controller writes `resource-quota` back into `spec.resourceQuota`.

An unrelated `ResourceQuota` in the same namespace is not selected or modified. To keep managing an existing quota, set its name explicitly in `spec.resourceQuota`.
If an unrelated object already uses the default `resource-quota` name, automatic creation fails instead of silently adopting it.

Example:

```yaml
apiVersion: dcn.ssu.ac.kr/v1
kind: QuotaAutoscaler
metadata:
  name: default-quotascale-controller
  namespace: default
spec:
  min.cpu: "4000m"
  max.cpu: "6"
  min.memory: "4G"
  max.memory: "6G"
  behavior:
    scaleDown:
      policies:
        - method: cpu
          value: 30
          targetUtilization: 50
        - method: memory
          value: 30
          targetUtilization: 50
    scaleUp:
      policies:
        - method: cpu
          value: 70
          targetUtilization: 50
        - method: memory
          value: 70
          targetUtilization: 50
```

Scaling behavior example:

- if CPU usage rises above `70%`, scale-up becomes eligible
- if CPU usage falls below `30%`, scale-down becomes eligible
- when scaling happens, quota is recalculated so current usage becomes `50%`

### 2. NodeScalingInventory

`NodeScalingInventory` is a cluster-scoped CRD in the `dcn.ssu.ac.kr/v1` API group.

Purpose:

- record the desired `MachineDeployment` replica count used for scaling nodes
- track observed scaling-related nodes and their usage state

Key spec fields:

- `machineDeploymentReplicas`: desired replica count from the Git-managed `MachineDeployment`
- `nodes[]`: tracked node list
  - `name`
  - `order`
  - `used`

This CRD is used internally by the node scaling controller.
The `nodes[]` list is rebuilt from actually observed scaling nodes, while preserving per-node `order` and `used` state for nodes that still exist.

## How quota scaling works

1. The controller watches `QuotaAutoscaler` and the referenced `ResourceQuota`.
2. It periodically checks quota usage and also listens for immediate quota-denied workload events.
3. It computes desired CPU and memory from current usage and the configured policy.
4. It clamps the result to `min/max` bounds.
5. If scale-up needs more cluster capacity than worker nodes can currently provide:
   - without node scaling enabled: the operation fails
   - with node scaling enabled: a scale-out request is sent to the node scaling controller first
6. The resize worker applies the quota change.

The current implementation manages CPU and memory only.

Important details:

- immediate event-driven scale-up is triggered only for quota-related `FailedCreate` events
- those events are filtered by process start time, so a freshly started controller does not replay old quota-denied events
- cluster fit checks use allocatable worker-node capacity minus currently requested pod resources
- control-plane nodes, unschedulable nodes, and nodes with the scaling `NoSchedule` taint are excluded from quota scale-up capacity

## How node scaling works

When node scaling is enabled:

- the controller clones a Git repository into `/tmp/quotascale-controller-node-scaling`
- it expects a `MachineDeployment` manifest at `feature/node-scaling/md.yaml` by default
- it changes `spec.replicas`, commits, and pushes the change

The node scaling controller also manages reserved scaling nodes by adding or removing the scaling label and `NoSchedule` taint as part of scale-out and scale-in workflows.
The maximum MachineDeployment replica count for node scaling is capped by `--node-scaling-max-nodes`, which defaults to `3`.

Important runtime behavior:

- if the manifest starts with `spec.replicas: 0`, startup baseline reconciliation changes it to `--node-scaling-prepared-spares`
- `spec.replicas` updates preserve the rest of the YAML document and write with 2-space indentation
- if the repo already exists locally and a pull fails, the controller can keep operating from the existing local checkout as long as the configured manifest is still readable
- git commit and push operations emit `INFO` logs that include the manifest path, repository, branch, and commit hash

### Scale-out sequence

1. The quota controller detects that desired quota exceeds current worker capacity.
2. The node scaling controller first tries to reuse nodes that are already in scale-in waiters.
3. If several waiters exist, the lower-order waiters are reused first and only the reused waiters are cancelled.
4. If more capacity is still needed, the controller activates additional unused prepared spare nodes from `NodeScalingInventory`, up to `--node-scaling-activated-spares` in one reconcile.
5. For every prepared spare node consumed this way, the controller increases the Git-managed `MachineDeployment` replica count to replenish the warm spare pool, updates `NodeScalingInventory`, commits, and pushes the manifest change.
6. If the configured `--node-scaling-max-nodes` cap is already reached, existing prepared spare nodes can still be activated, but spare-pool replenishment is capped by that maximum.

### Scale-in sequence

1. Automatic scale-in becomes eligible only after managed quota limits fit within non-scaling capacity for `--node-scale-in-delay`.
2. Existing unused scaling nodes are removed from desired `MachineDeployment` replicas first, but never below `--node-scaling-prepared-spares`.
3. Additional used scaling nodes can be reserved in parallel for future scale-in.
4. Reserved nodes are marked with the scaling label and `NoSchedule` taint, then watched asynchronously until they no longer have blocking pods.
5. When a reserved node drains, it is marked unused in `NodeScalingInventory` and another scale-in reconcile is queued.
6. If `--enable-node-scale-in-force=true`, a waiter can also be force-completed after `--node-scale-in-force-delay` even when blocking pods still remain.
7. If `--enable-node-scale-in-force=false`, the waiter remains active indefinitely until the blocking pods are gone.
8. If a new scale-out request arrives while scale-in waiters are active, only the reused lower-order waiter is cancelled; the other waiters continue.
9. The same prepared-spare minimum also applies to automatic scale-in eligibility, so the controller does not keep scaling in once it has reached the configured warm spare floor.

### What counts as a blocking pod during scale-in

Reserved scale-in nodes are considered drained only when no blocking pods remain.

Ignored by default:

- terminal pods in `Succeeded` or `Failed`
- pods already being deleted
- mirror/static pods
- `DaemonSet` pods
- pods in namespaces listed by `--node-scale-in-exempt-namespaces`
- pods with the configured exempt label or annotation marker

Blocking by default:

- regular workload pods that are still running on the reserved node
- middleware or platform pods that are not exempted by namespace or marker

If `--enable-node-scale-in-force=true` and blocking pods remain for longer than `--node-scale-in-force-delay`, the controller force-completes that node's scale-in path without using the eviction API.
If `--enable-node-scale-in-force=false`, the controller waits indefinitely and relies on future pod drainage or waiter reuse by a later scale-out request.

## Running locally

Apply the CRDs and example manifests:

```sh
export KUBECONFIG=$HOME/.kube/config

kubectl apply -f deploy/helm-quotascale-controller/templates/crd.yaml
kubectl apply -f example/example-scaler.yaml
```

Run the controller:

```sh
go run ./cmd/quotascale-controller \
  --quota-check-interval=30s \
  --quota-update-interval=1m
```

If the CRD is missing, the controller exits with an error such as:

```text
the server could not find the requested resource (get quotaautoscalers.dcn.ssu.ac.kr)
```

In that case, install the CRD first.

## Runtime flags

| Flag | Meaning | Default |
| --- | --- | --- |
| `--enable-node-scaling` | Enable the node scaling controller | `false` |
| `--quota-check-interval` | Periodic quota utilization check interval | `1m` |
| `--quota-update-interval` | Minimum delay between resize operations for the same namespace | `1m` |
| `--node-scale-in-delay` | How long scale-in eligibility must remain true before automatic scale-in | `5m` |
| `--enable-node-scale-in-force` | Enable forced completion of scale-in waiters after the configured force delay | `true` |
| `--node-scale-in-force-delay` | How long a reserved scale-in node may keep blocking pods before forced scale-in path continues | `5m` |
| `--node-scale-in-exempt-namespaces` | Comma-separated namespaces whose pods do not block scale-in | `kube-system` |
| `--node-scale-in-exempt-pod-key` | Label or annotation key that marks a pod as scale-in exempt | `quotascale.dcn.ssu.ac.kr/scale-in-exempt` |
| `--node-scale-in-exempt-pod-value` | Label or annotation value that marks a pod as scale-in exempt | `true` |
| `--node-scaling-max-nodes` | Maximum MachineDeployment replica count allowed for node scaling | `3` |
| `--node-scaling-prepared-spares` | Minimum number of warm spare scaling nodes that should remain prepared | `1` |
| `--node-scaling-activated-spares` | Maximum number of prepared spare nodes to activate in a single scale-out reconcile | `1` |
| `--node-scaling-repo-url` | Git repository URL for node scaling manifests | `""` |
| `--node-scaling-repo-branch` | Git branch for node scaling manifests | `""` |
| `--node-scaling-repo-file-path` | Path to the MachineDeployment manifest inside the node scaling repo | `""` |
| `--node-scaling-git-username` | Git username for node scaling repo access | `""` |
| `--node-scaling-git-password` | Git password or token for node scaling repo access | `""` |

## Node scaling GitOps configuration

The current GitOps integration is intended to run on top of Argo CD.
In other words, this controller updates a Git-managed `MachineDeployment` manifest, and Argo CD is expected to reconcile that repository change into the workload cluster.

At this stage, the README deliberately does not lock down the full GitOps operating contract yet.
That means the repository layout, Argo CD `Application` manifests, bootstrap sequence, credentials wiring, and any supporting files required for a production GitOps flow are not treated as finalized in this document.
Those details will be added later once the end-to-end GitOps logic is finalized.

When `--enable-node-scaling=true`, the controller reads the following environment variables:

- `GITEA_REPO_URL`
- `GITEA_REPO_BRANCH` optional, default `main`
- `GITEA_REPO_FILE_PATH` optional, default `feature/node-scaling/md.yaml`
- `GITEA_USERNAME`
- `GITEA_PASSWORD`

The same node-scaling Git settings can also be provided with runtime flags.
When both are set, the runtime flags take precedence over the environment
variables.

The referenced file must be a CAPI `MachineDeployment` manifest.
If `GITEA_REPO_URL` uses HTTP basic authentication, the controller uses `GITEA_USERNAME` and `GITEA_PASSWORD` for clone, pull, commit, and push operations.

## Example manifests

- `example/example-scaler.yaml`: example `QuotaAutoscaler`
- `example/test/`: full demo scenario with namespaces, workloads, HPA, and Locust-based replay traffic

## RBAC summary

The controller needs cluster-level access for:

- `quotaautoscalers.dcn.ssu.ac.kr`
- `nodescalinginventories.dcn.ssu.ac.kr`
- `resourcequotas`
- `events`
- `nodes`
- `pods`
- workload owners needed for quota-denied event resolution:
  - `replicasets`
  - `replicationcontrollers`
  - `statefulsets`
  - `jobs`

If you use the stub resize implementation, `patch` permission on `resourcequotas` is also required.

## Current scope and limitations

- CPU and memory are supported
- storage is not scaled
- quota-denied event handling is focused on workload creation failures
- `DaemonSet` workloads are not part of quota expansion logic
- node scaling behavior assumes a GitOps-managed `MachineDeployment`
- force-completing a timed-out scale-in waiter does not evict pods; it advances controller state so infrastructure scaling can continue

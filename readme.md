# QuotaScale Controller

QuotaScale Controller is a Kubernetes controller that automatically adjusts namespace `ResourceQuota` for CPU and memory. It watches a custom resource named `QuotaAutoscaler`, tracks the target namespace quota usage, reacts immediately to quota-denied workload creation events, and updates the managed `ResourceQuota`.

When node scaling is enabled, the controller also coordinates a second control loop for scaling-dedicated nodes through GitOps.

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

### Resize worker

The resize worker serializes quota updates per namespace and rate-limits repeated resize operations.

By default, the stub implementation patches the target `ResourceQuota` directly. The implementation lives in `internal/resize/api.go`.

### Node scaling controller

The node scaling controller is optional and is enabled with `--enable-node-scaling=true`.

Its responsibilities are:

- maintain an inventory of scaling-related nodes
- activate a reserved scaling node when quota scale-out needs more cluster capacity
- update a Git-managed `MachineDeployment` replica count
- evaluate automatic scale-in after managed quota totals fit without scaling nodes for a configured delay

## Custom resources

### 1. QuotaAutoscaler

`QuotaAutoscaler` is a namespaced CRD in the `dcn.ssu.ac.kr/v1` API group.

Purpose:

- declare which `ResourceQuota` should be managed
- define scale-up and scale-down thresholds
- define the target utilization after scaling
- define CPU and memory bounds

Key spec fields:

- `resourceQuota`: target `ResourceQuota` name in the same namespace
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

Example:

```yaml
apiVersion: dcn.ssu.ac.kr/v1
kind: QuotaAutoscaler
metadata:
  name: default-quotascale-controller
  namespace: default
spec:
  resourceQuota: compute-resources
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
- track scaling-related nodes and their usage state

Key spec fields:

- `machineDeploymentReplicas`: desired replica count from the Git-managed `MachineDeployment`
- `nodes[]`: tracked node list
  - `name`
  - `order`
  - `used`

This CRD is used internally by the node scaling controller.

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

## How node scaling works

When node scaling is enabled:

- the controller clones a Git repository into `/tmp/quotascale-controller-node-scaling`
- it expects a `MachineDeployment` manifest at `feature/node-scaling/md.yaml` by default
- it changes `spec.replicas`, commits, and pushes the change

The node scaling controller also manages reserved scaling nodes by adding or removing the scaling label and `NoSchedule` taint as part of scale-out and scale-in workflows.

## Running locally

Apply the CRDs and example manifests:

```sh
export KUBECONFIG=$HOME/.kube/config

kubectl apply -f deploy/helm-quotascale-controller/templates/crd.yaml
kubectl apply -f example/compute-resources.yaml
kubectl apply -f example/example-scaler.yaml
kubectl apply -f example/test-workload.yaml
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

## Node scaling GitOps configuration

When `--enable-node-scaling=true`, the controller reads the following environment variables:

- `GITEA_REPO_URL`
- `GITEA_REPO_BRANCH` optional, default `main`
- `GITEA_REPO_FILE_PATH` optional, default `feature/node-scaling/md.yaml`
- `GITEA_USERNAME`
- `GITEA_PASSWORD`

The referenced file must be a CAPI `MachineDeployment` manifest.

## Example manifests

- `example/compute-resources.yaml`: example `ResourceQuota`
- `example/example-scaler.yaml`: example `QuotaAutoscaler`
- `example/test-workload.yaml`: simple test deployment for quota pressure

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

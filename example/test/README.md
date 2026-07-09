# QuotaScale Controller Demo Test Set

This directory contains a full demo scenario for exercising:

- namespace-local HPA scale-out and scale-in
- quota scale-up and scale-down by `QuotaAutoscaler`
- pressure across two namespaces that can eventually justify node scaling

The traffic story is intentionally trace-based rather than hand-made.
The Locust input curves in `example/test/locust` were derived from the public
`1998 World Cup Web Site Access Logs` published by the Internet Traffic Archive:

- https://ita.ee.lbl.gov/html/contrib/WorldCup.html

The goal is that a third party can look at this folder and see that:

- the Kubernetes resources are simple and explicit
- the traffic source is public and cited
- the A/B traffic files are reproducible from official log files
- the Locust execution model matches the claim being demonstrated

## Fresh Cluster Assumption

This guide assumes a very fresh Kubernetes cluster where the base control plane
is already working and a CNI plugin has been installed, but common add-ons such
as `metrics-server` are not installed yet.

Before applying the demo manifests, make sure you have:

- working `kubectl` access to the cluster
- a CNI plugin already installed
- DNS working inside the cluster
- outbound image pulls allowed from your nodes

## Install Metrics Server

This demo uses CPU-based HPA, so `metrics-server` must be installed first.
The official Metrics Server installation command is:

```sh
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
```

After installation, verify that the deployment is ready:

```sh
kubectl -n kube-system rollout status deploy/metrics-server
kubectl get apiservice v1beta1.metrics.k8s.io
kubectl top nodes
```

If `kubectl top nodes` or `kubectl top pods` fails in a lab or self-signed
environment because kubelet certificates are not signed by the cluster CA,
Metrics Server's own documentation notes that you may need to run it with
`--kubelet-insecure-tls`. In that case, patch the deployment after install:

```sh
kubectl -n kube-system patch deployment metrics-server \
  --type='json' \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

Then restart-check it:

```sh
kubectl -n kube-system rollout status deploy/metrics-server
kubectl top nodes
```

## Install Locust

Locust runs outside the cluster on the traffic generator host or VM.
The official installation flow is:

```sh
python3 -m venv .venv
source .venv/bin/activate
pip install --upgrade pip
pip install locust
locust -V
```

If you do not want a virtual environment, the minimal official install command
is:

```sh
pip install locust
```

For this demo, running Locust in a virtual environment is recommended so the
traffic generator machine stays isolated from system Python packages.

## Scenario Summary

- Namespace A: `quota-test-a`
- Namespace B: `quota-test-b`
- `ResourceQuota` in each namespace:
  - `requests.cpu: 4`
  - `limits.cpu: 4`
  - `requests.memory: 4Gi`
  - `limits.memory: 4Gi`
- `QuotaAutoscaler` in each namespace:
  - `max.cpu: 15`
  - `max.memory: 15Gi`
- Workload pod sizing in each namespace:
  - `requests.cpu: 1`
  - `limits.cpu: 1`
  - `requests.memory: 1Gi`
  - `limits.memory: 1Gi`
- HPA targets:
  - namespace A: `70%` CPU
  - namespace B: `50%` CPU
  - `maxReplicas: 15`

## Workloads

- Namespace A runs a CPU-heavy SHA-256 loop HTTP service
- Namespace B runs a CPU-heavy PBKDF2 HTTP service

These were chosen because the demo goal is HPA scale-out on CPU usage while
keeping pod sizing fixed at `1 CPU` and `1Gi`. Both services are HTTP-based and
intentionally CPU-heavy per request, which is much better aligned with this
goal than lightweight health/demo services.

## Files

- `00-namespaces.yaml`: namespaces
- `10-resourcequotas.yaml`: per-namespace `ResourceQuota`
- `20-quotaautoscalers.yaml`: per-namespace `QuotaAutoscaler`
- `25-workload-configmaps.yaml`: Python HTTP server scripts for the CPU-heavy test workloads
- `30-workloads.yaml`: CPU-heavy HTTP workload deployments
- `35-services.yaml`: `NodePort` Services for traffic generation from an external Locust VM
- `40-hpa.yaml`: per-namespace HPA
- `locust/build_worldcup_trace.py`: converts official World Cup binary logs into per-second CSV traces
- `locust/worldcup_a.csv`: namespace A traffic trace
- `locust/worldcup_b.csv`: namespace B traffic trace
- `locust/worldcup_a.summary.txt`: provenance for namespace A trace
- `locust/worldcup_b.summary.txt`: provenance for namespace B trace
- `locust/locustfile.py`: replay runner
- `locust/run_dual_replay.sh`: convenience launcher for two Locust processes
- `locust/stop_dual_replay.sh`: stops the two replay processes created by the launcher

## Traffic Provenance

The generated traces in this folder are not arbitrary ramps.

Namespace A trace:

- source file: `wc_day46_1.gz`
- dataset interpretation: opening-day traffic, from the first match day of the tournament
- official source date: June 10, 1998
- selected local-time window: `1998-06-10T08:12:02+02:00` to `1998-06-10T08:42:01+02:00`
- normalized peak: `9 RPS`
- output file: `locust/worldcup_a.csv`

Namespace B trace:

- source file: `wc_day66_1.gz`
- dataset interpretation: knockout-stage traffic, from the round-of-16 period of the tournament
- official source date: June 30, 1998
- selected local-time window: `1998-06-30T00:00:06+02:00` to `1998-06-30T00:30:05+02:00`
- normalized peak: `12 RPS`
- output file: `locust/worldcup_b.csv`

The normalization keeps the original burst shape but scales the absolute
throughput down to a demo-safe level.

This choice is intentional:

- namespace A uses an opening-day slice so the demo includes a trace tied to the start of the event
- namespace B uses a knockout-stage slice so the demo also includes a trace from a later, higher-interest tournament phase
- together they show that the replay inputs were selected from meaningful points in the public dataset, not invented ad hoc

The exact provenance is stored in:

- `locust/worldcup_a.summary.txt`
- `locust/worldcup_b.summary.txt`

## Why Two Locust Processes

Locust can represent multiple user classes in one test file, and each user
class can target a different host. However, the official custom shape model
controls one runner-wide user count and spawn rate over time. In other words,
the built-in `LoadTestShape.tick()` contract gives one time-series for the
runner, not two independent time-series with separate counts for A and B.

Official references:

- Custom load shapes:
  - https://docs.locust.io/en/stable/custom-load-shape.html
- Writing a locustfile:
  - https://docs.locust.io/en/stable/writing-a-locustfile.html

Because this demo needs two different traffic curves, the clearer and more
defensible setup is:

- one headless Locust process for namespace A replay
- one headless Locust process for namespace B replay

That keeps the logic simple:

- one process
- one service
- one World Cup-derived curve

## How The Traffic Generator Reaches The Workloads

The demo assumes Locust runs on a separate traffic-generator VM outside the
cluster. In that setup, Kubernetes internal FQDNs such as
`quota-test-app-a.quota-test-a.svc.cluster.local` are not directly reachable
from the VM.

For that reason, `example/test/35-services.yaml` exposes both workloads as
`NodePort` Services:

- namespace A: `http://<NODE_IP>:30080`
- namespace B: `http://<NODE_IP>:30081`

Use any reachable Kubernetes node IP for `<NODE_IP>` from the traffic
generator VM. The `run_dual_replay.sh` helper script uses `NODE_IP` and these
NodePort values by default.

## How The Replay Works

`locust/locustfile.py` replays a per-second CSV with columns:

```csv
second,rps
0,6.84
1,6.90
2,6.94
```

The file contains normalized `RPS` values derived from the World Cup logs.
The Locust runner buckets that signal into `30s` steps and converts each step
into a target user count. This is intentionally less twitchy than changing the
user count every second, which previously caused unstable ramp-up and shutdown
behavior in Locust.

Each Locust user continuously issues CPU-heavy HTTP requests with no think time:

- namespace A hits `/burn?rounds=240000`
- namespace B hits `/derive?iterations=350000`

`USER_SCALE` then scales the replay intensity up or down while keeping the
World Cup-derived shape.

This means:

- the curve shape comes from real data
- the absolute load is adapted to the cluster
- the scaling factor is explicit, not hidden

## Rebuilding The Trace Files

The trace CSVs in this folder were built from the official ITA logs.
If you want to regenerate them:

```sh
curl -L -o /tmp/wc_day46_1.gz https://ita.ee.lbl.gov/traces/WorldCup/wc_day46_1.gz
curl -L -o /tmp/wc_day66_1.gz https://ita.ee.lbl.gov/traces/WorldCup/wc_day66_1.gz

python3 example/test/locust/build_worldcup_trace.py \
  --input /tmp/wc_day46_1.gz \
  --output example/test/locust/worldcup_a.csv \
  --summary example/test/locust/worldcup_a.summary.txt \
  --window-seconds 1800 \
  --peak-rps 9

python3 example/test/locust/build_worldcup_trace.py \
  --input /tmp/wc_day66_1.gz \
  --output example/test/locust/worldcup_b.csv \
  --summary example/test/locust/worldcup_b.summary.txt \
  --window-seconds 1800 \
  --peak-rps 12
```

What the builder does:

- parses the official binary log format directly
- counts requests per second
- finds the busiest `window-seconds` interval in the source file
- smooths the series with a short moving average
- scales the peak to the requested value

## Apply Order

```sh
kubectl apply -f example/test/00-namespaces.yaml
kubectl apply -f example/test/10-resourcequotas.yaml
kubectl apply -f example/test/20-quotaautoscalers.yaml
kubectl apply -f example/test/25-workload-configmaps.yaml
kubectl apply -f example/test/30-workloads.yaml
kubectl apply -f example/test/35-services.yaml
kubectl apply -f example/test/40-hpa.yaml
```

## Pre-Experiment Checklist

- `QuotaAutoscaler` CRD is installed
- `metrics-server` or equivalent metrics source is working for HPA
- `kubectl top nodes` and `kubectl top pods -A` both succeed
- the QuotaScale controller is running
- CPU-heavy workload pods are Ready
- `quota-test-app-a` is reachable at `http://<NODE_IP>:30080`
- `quota-test-app-b` is reachable at `http://<NODE_IP>:30081`
- namespace quotas exist before traffic starts
- Locust is installed in the traffic generator environment
- `locust -V` succeeds on the traffic generator machine
- the traffic generator VM can connect to:
  - `<NODE_IP>:30080`
  - `<NODE_IP>:30081`

## Launch Plan Just Before The Experiment

Run namespace A replay:

```sh
WORLD_CUP_TRACE_CSV=example/test/locust/worldcup_a.csv \
USER_SCALE=12.0 \
NODE_IP=<REACHABLE_K8S_NODE_IP> \
locust -f example/test/locust/locustfile.py \
  --headless \
  --host http://<REACHABLE_K8S_NODE_IP>:30080 \
  CpuBurnAUser
```

Run namespace B replay:

```sh
WORLD_CUP_TRACE_CSV=example/test/locust/worldcup_b.csv \
USER_SCALE=12.0 \
NODE_IP=<REACHABLE_K8S_NODE_IP> \
locust -f example/test/locust/locustfile.py \
  --headless \
  --host http://<REACHABLE_K8S_NODE_IP>:30081 \
  CpuBurnBUser
```

Or use:

```sh
NODE_IP=<REACHABLE_K8S_NODE_IP> \
bash example/test/locust/run_dual_replay.sh
```

Stop both replay processes:

```sh
bash example/test/locust/stop_dual_replay.sh
```

This repository does not currently have `locust` installed locally, so the
commands above are prepared but were not executed from this workspace.

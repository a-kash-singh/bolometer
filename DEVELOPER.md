# Bolometer — Developer Guide

> *How it works, how to test it locally, and how to extend it.*

---

## What is Bolometer?

Bolometer is a Kubernetes operator (CRD controller) that watches Go applications running in your cluster and automatically captures [pprof](https://pkg.go.dev/net/http/pprof) profiles when they run hot — either because resource usage exceeds a threshold, or on a fixed schedule.

Profiles are uploaded to S3 (or any S3-compatible store) for later analysis.

```
┌─────────────────────────────────────────────────────────────────┐
│                        Your K8s Cluster                         │
│                                                                  │
│  ┌──────────────────────────────────────────────────────────┐  │
│  │  Bolometer Operator (bolometer-system namespace)          │  │
│  │                                                           │  │
│  │  ProfilingConfigReconciler                                │  │
│  │    │                                                      │  │
│  │    ├─ PodWatcher      — finds annotated pods              │  │
│  │    ├─ MetricsCollector — queries metrics-server           │  │
│  │    ├─ Profiler        — port-forwards + calls pprof HTTP  │  │
│  │    └─ S3Uploader      — uploads .pprof files to S3        │  │
│  └──────────────────────────────────────────────────────────┘  │
│                                                                  │
│  ┌─────────────────┐      ┌─────────────────────────────────┐  │
│  │  Your Go App    │      │  ProfilingConfig (CRD)           │  │
│  │                 │      │                                   │  │
│  │  annotations:   │      │  spec:                           │  │
│  │   bolometer.io/ │      │    selector: {app: my-app}       │  │
│  │     enabled:true│      │    thresholds: {cpu: 80%}        │  │
│  │   bolometer.io/ │      │    onDemand: {enabled: true}     │  │
│  │     port: 6060  │      │    s3Config: {bucket: ...}       │  │
│  │  /debug/pprof/✓ │      │                                   │  │
│  └─────────────────┘      └─────────────────────────────────┘  │
└─────────────────────────────────────────────────────────────────┘
                                      │
                               ┌──────▼──────┐
                               │  S3 Bucket  │
                               │ profiles/   │
                               │ 2024-01-15/ │
                               │   my-app/   │
                               │   heap.pprof│
                               └─────────────┘
```

---

## Key Concepts

### The CRD: `ProfilingConfig`

A `ProfilingConfig` is a namespaced Kubernetes resource that tells Bolometer:

1. **Which pods to watch** — by namespace + label selector. The pod must also have `bolometer.io/enabled: "true"`.
2. **When to profile** — CPU/memory thresholds _and/or_ a fixed on-demand interval.
3. **Where to upload** — S3 bucket, prefix, region, and optional custom endpoint (MinIO etc).
4. **What to capture** — heap, cpu, goroutine, mutex (any subset).

```yaml
apiVersion: bolometer.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: my-config
  namespace: default
spec:
  selector:
    namespace: default
    labelSelector:
      app: my-go-app           # must also have bolometer.io/enabled: "true"

  thresholds:
    cpuThresholdPercent: 80    # fire when CPU > 80% of pod request
    memoryThresholdPercent: 90
    checkIntervalSeconds: 30
    cooldownSeconds: 300       # min gap between profiles per pod

  onDemand:                    # optional: profile every N seconds regardless
    enabled: true
    intervalSeconds: 35

  s3Config:
    bucket: my-bucket
    prefix: profiles
    region: us-west-2
    endpoint: ""               # leave empty for AWS; set for MinIO etc

  profileTypes: [heap, cpu, goroutine, mutex]
```

### Pod Annotation Requirements

Your Go app pods need two things:

```yaml
metadata:
  annotations:
    bolometer.io/enabled: "true"   # required
    bolometer.io/port: "6060"      # optional, default 6060
```

And the app must serve pprof (import side-effect only):

```go
import _ "net/http/pprof"

go http.ListenAndServe(":6060", nil)
```

---

## Architecture Deep-Dive

### Controller Loop

Every 30 seconds (or on any `ProfilingConfig` change), the reconciler:

```
Reconcile()
  ├─ Fetch ProfilingConfig from API server
  ├─ Validate (bucket + region required)
  ├─ PodWatcher.ListMatchingPods()    — list pods by label + annotation filter
  ├─ Update status.activePods
  ├─ stopMonitoring(old)             — cancel previous goroutines for this config
  └─ startMonitoring(new)
       ├─ go monitorThresholds()     — loop: check metrics → capture if exceeded
       └─ go monitorOnDemand()       — loop: capture every N seconds (if enabled)
```

### How Profiling Works

`Profiler.CaptureProfiles()` for each target pod:

1. Opens a `kubectl port-forward` via the SPDY protocol (same as `kubectl port-forward` on the CLI but in-process).
2. Picks a random free local port.
3. Sends HTTP GET to `localhost:{port}/debug/pprof/{type}`.
4. CPU profile blocks for 30 seconds (that's how pprof works).
5. Returns raw binary data.

Then `S3Uploader.UploadProfiles()` uploads each file with key:
```
{prefix}/{YYYY-MM-DD}/{service-name}/{YYYYMMDDHHmmss}-{type}.pprof
```

### Context Lifetime for Monitoring Goroutines

A key subtlety: `Reconcile(ctx, req)` receives a **per-request context** that controller-runtime cancels as soon as `Reconcile()` returns. Passing that context to the background monitoring goroutines would kill them immediately (and any in-flight `pprof` capture — which can take 30+ seconds for CPU profiles).

The reconciler owns a `baseCtx` (derived from `context.Background()`) that outlives any single reconcile call. All monitoring goroutines are started with `context.WithCancel(r.baseCtx)`. Individual monitors are stopped by calling their cancel function (stored in `activeMonitors`). The entire `baseCtx` is cancelled when the manager shuts down via a `Runnable` registered in `SetupWithManager`.

### Thread Safety

`PodWatcher` uses a `sync.RWMutex` to protect its pod maps — read operations (GetTrackedPods, CanProfile) take a read lock; writes (TrackPod, UpdateLastProfileTime) take an exclusive lock.

The `activeMonitors` map in the reconciler is **not** concurrently accessed — it's only touched during `Reconcile()` which controller-runtime serializes per object.

### Metrics Calculation

CPU/memory thresholds are percentages of **pod resource requests** (not limits, not node capacity):

```
cpuPercent = (actual CPU usage in millicores) / (requested CPU in millicores) × 100
```

If a pod has no resource requests, percentages are 0 and threshold profiling never fires. Set requests on your pods.

---

## Project Layout

```
bolometer/
├── api/v1alpha1/
│   ├── profilingconfig_types.go    # CRD struct definitions (source of truth)
│   └── zz_generated.deepcopy.go   # auto-generated, don't edit
│
├── internal/
│   ├── controller/
│   │   ├── profilingconfig_controller.go  # main reconcile loop
│   │   └── pod_watcher.go                 # pod tracking + cooldown
│   ├── metrics/
│   │   └── collector.go                   # metrics-server client + threshold check
│   ├── profiler/
│   │   └── profiler.go                    # port-forward + pprof HTTP capture
│   └── uploader/
│       └── s3.go                          # S3 upload + S3 key generation
│
├── cmd/main.go                    # operator entrypoint, wires everything together
├── config/
│   ├── crd/                       # CRD YAML (apply this to install)
│   ├── rbac/                      # ClusterRole, ClusterRoleBinding, ServiceAccount
│   ├── manager/                   # operator Deployment + Kustomize config
│   └── samples/                   # example ProfilingConfig YAMLs
├── examples/
│   ├── sample-app/                # a Go app with pprof already wired up
│   └── target-app.yaml            # deployment YAML for the sample app
├── helm/bolometer/                # Helm chart (recommended for production)
│
├── Makefile                       # build, test, deploy shortcuts
├── e2e-local.sh                   # full local e2e test (kind + MinIO)
└── DEVELOPER.md                   # this file
```

---

## Running Tests

### Unit Tests (no cluster needed)

```bash
# All tests
make test

# With verbose output
go test ./... -v

# Single package
go test ./internal/controller/... -v
go test ./internal/metrics/... -v
go test ./internal/profiler/... -v
go test ./internal/uploader/... -v

# Coverage report
make test-coverage          # prints % to terminal
make test-coverage-html     # opens coverage.html
```

The unit tests use fake Kubernetes clients (`k8s.io/client-go/kubernetes/fake` and `sigs.k8s.io/controller-runtime/pkg/client/fake`) — no cluster required.

### What's Tested

| Package | Tests |
|---|---|
| `controller` | Reconcile lifecycle, config validation, pod filtering, status updates, monitoring start/stop, namespace isolation |
| `metrics` | Threshold comparison, CPU/memory percentage calculation with edge cases |
| `profiler` | pprof port resolution (annotation parsing), profile endpoint URLs |
| `uploader` | S3 key generation, service name extraction from labels/owner refs |

### End-to-End Test (local cluster)

Prerequisites: **docker, kind, kubectl, go, helm**

```bash
chmod +x e2e-local.sh
./e2e-local.sh
```

The script:
1. Creates a `kind` cluster named `bolometer-e2e`
2. Deploys MinIO as a local S3 backend
3. Builds the operator binary + Docker image, loads into kind
4. Deploys the CRD, RBAC, and operator
5. Deploys the sample Go pprof app
6. Creates a `ProfilingConfig` with low thresholds (fires immediately) + on-demand mode
7. Waits up to 2 minutes for profiles to appear
8. Prints a summary with operator logs and MinIO bucket contents

```bash
# Flags
./e2e-local.sh --cleanup     # delete the kind cluster when done
./e2e-local.sh --skip-build  # reuse existing Docker image (faster iteration)
```

After the script, you can keep experimenting:

```bash
# Watch the operator in real time
kubectl logs -n bolometer-system -l app=bolometer -f

# Check ProfilingConfig status
kubectl describe profilingconfig e2e-test -n demo

# Open MinIO console to browse uploaded profiles
kubectl port-forward svc/minio 9001:9001 -n minio
# → http://localhost:9001  (user: minioadmin / pass: minioadmin)
```

---

## Local Development (without a cluster)

```bash
# Download dependencies
make deps

# Format + vet
make fmt vet

# Build binary
make build          # outputs bin/manager

# Run operator against your current kubeconfig
make run            # needs a cluster with the CRD installed
```

To install the CRD in your cluster before running locally:

```bash
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
make run
```

---

## Production Deployment (EKS + IRSA)

The recommended production path is Helm + IRSA (no static credentials ever leave your cluster).

### Quick start (3 steps)

**Step 1 — Set up IRSA** (one-time, needs AWS CLI):

```bash
CLUSTER_NAME=my-cluster \
AWS_REGION=us-west-2 \
S3_BUCKET=my-pprof-bucket \
./aws/irsa-setup.sh
```

This creates the IAM policy (`aws/iam-policy.json`) and an IAM role with the correct OIDC trust relationship, then prints the `helm install` command for Step 3.

**Step 2 — Push your image**:

```bash
docker build -t $ACCOUNT.dkr.ecr.$REGION.amazonaws.com/bolometer:0.1.0 .
docker push $ACCOUNT.dkr.ecr.$REGION.amazonaws.com/bolometer:0.1.0
```

**Step 3 — Install with Helm**:

```bash
helm upgrade --install bolometer ./helm/bolometer \
  -n bolometer-system --create-namespace \
  -f ./helm/bolometer/values-aws.yaml \
  --set aws.irsa.roleArn=arn:aws:iam::ACCOUNT:role/bolometer-role \
  --set aws.region=us-west-2 \
  --set image.repository=ACCOUNT.dkr.ecr.REGION.amazonaws.com/bolometer \
  --set image.tag=0.1.0
```

### Key Helm values

| Value | Default | Description |
|---|---|---|
| `aws.irsa.enabled` | `false` | Enable IRSA (adds OIDC annotation to ServiceAccount) |
| `aws.irsa.roleArn` | `""` | IAM role ARN for IRSA |
| `aws.region` | `us-east-1` | AWS region (injected as `AWS_REGION` env var) |
| `aws.staticCredentials.enabled` | `false` | Static creds for MinIO / non-EKS setups |
| `aws.staticCredentials.existingSecret` | `""` | Reference a pre-created Secret instead of creating one |
| `leaderElection.enabled` | `true` | Disable for single-node testing |
| `profilingConfig.create` | `false` | Deploy a ProfilingConfig CR via this Helm release |

See `helm/bolometer/values.yaml` for all options, `values-aws.yaml` for a production example, and `values-minio.yaml` for local MinIO testing.

### IAM policy

Minimum S3 permissions (`aws/iam-policy.json`):
- `s3:PutObject` + `s3:GetObject` on `arn:aws:s3:::YOUR-BUCKET/*`
- `s3:ListBucket` + `s3:GetBucketLocation` on `arn:aws:s3:::YOUR-BUCKET`

### Via kubectl (without Helm)

```bash
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
kubectl apply -f config/manager/
```

---

## Target App Requirements Checklist

- [ ] Import `_ "net/http/pprof"` in your Go binary
- [ ] Expose pprof on a port (default 6060): `go http.ListenAndServe(":6060", nil)`
- [ ] Add annotation `bolometer.io/enabled: "true"` to pod template
- [ ] Add annotation `bolometer.io/port: "6060"` if using a non-default port
- [ ] Set CPU and memory **requests** on your containers (thresholds are % of requests)
- [ ] Pod must be in `Running` phase

---

## Troubleshooting

> **Note:** Some older docs show `profiling.io/enabled` — this is wrong. The correct annotations are `bolometer.io/enabled` and `bolometer.io/port` (matching the code and CRD group `bolometer.io`).

**Operator can't find pods**
```bash
kubectl get pods -n <target-ns> -o jsonpath='{range .items[*]}{.metadata.name}: {.metadata.annotations.bolometer\.io/enabled}{"\n"}{end}'
```

**Profiles not uploading**
```bash
# Check operator logs for S3 errors
kubectl logs -n bolometer-system -l app=bolometer | grep -i "upload\|s3\|error"
```

**Threshold never fires**
- Check that pods have CPU/memory **requests** set
- Lower thresholds temporarily for testing: `cpuThresholdPercent: 1`
- Use on-demand mode (`onDemand.enabled: true`) as a bypass

**pprof endpoint unreachable**
```bash
# Verify pprof is running in the pod
kubectl exec <pod> -- wget -qO- http://localhost:6060/debug/pprof/ 2>&1
```

**metrics-server not available**
```bash
kubectl top pods  # should work if metrics-server is installed
# Install: kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
```

---

## Extending Bolometer

**Add a new profile type**: add the URL mapping in `profiler.go:getProfileEndpoint()`.

**Support non-Go apps**: replace the pprof HTTP call in `profiler.go:captureProfile()` with the appropriate profiling protocol.

**Different storage backends**: implement the same interface as `S3Uploader` and swap in `captureAndUpload()` in the controller.

**Alert on threshold violations**: add an event recorder call in `checkPodsThresholds()` after the threshold fires.

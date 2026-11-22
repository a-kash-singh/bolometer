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

Every 30 seconds (or on any `ProfilingConfig` change, including status updates), the reconciler:

```
Reconcile()
  ├─ Fetch ProfilingConfig from API server
  ├─ Validate (bucket + region required)
  │    └─ invalid: stop monitor, Ready=False + Warning event, return (see below)
  ├─ PodWatcher.ListMatchingPods()    — list pods by label + annotation filter
  ├─ PodWatcher.SyncPods()            — make the matching pods this config's tracked set
  ├─ Update status.activePods and Ready=True
  └─ ensureMonitoring()
       ├─ first time: start a configMonitor
       │    ├─ go monitorThresholds() — every checkIntervalSeconds: check metrics → capture if exceeded
       │    └─ go monitorOnDemand()   — every onDemand.intervalSeconds (idles while disabled)
       └─ afterwards: hand the latest config to the running monitor
```

The monitor is **not** restarted on reconcile. Reconciles are frequent (the
30s requeue plus one per status update), and restarting would reset the
schedules and cancel in-flight captures. Instead:

- Each schedule times its next run from its previous run, so reconciles cannot
  postpone it. Intervals longer than the requeue work as configured.
- A spec edit wakes the schedules, which re-read the config and re-evaluate
  their deadline: a shortened interval that is already overdue runs
  immediately, a lengthened one pushes the next run out, and enabling
  on-demand starts its first interval.
- Each capture runs with the latest spec (thresholds, S3 config, profile types).
- Only deleting the `ProfilingConfig` (or manager shutdown) stops the monitor
  and cancels in-flight captures.

Pods are tracked **per ProfilingConfig**. Each config's threshold and
on-demand monitors only check and capture the pods that config selected, even
when several configs select pods in the same namespace. On every reconcile a
config's tracked set is replaced by the pods currently matching it. Pods that
were deleted, lost the label or `bolometer.io/enabled: "true"` annotation, or
left the `Running` phase are dropped. Deleting a config drops all of its pods.
Pods aren't watched, so a deleted pod is dropped at the config's next
reconcile (within the 30s requeue).

The per-pod cooldown (`cooldownSeconds`) starts after a successful
threshold-triggered capture. It survives re-tracking and is shared by the
configs that select the pod. Its state is dropped once no config tracks the
pod any more. On-demand captures don't start a cooldown. Only one
capture per pod runs at a time, because pprof refuses a second concurrent CPU
profile. A schedule that finds the pod busy skips it for that run.

Captures run in the background, for both schedules. A threshold pass only
checks metrics and starts captures, and an on-demand run only starts
captures, so both finish promptly and every pod stays on schedule. A slow
capture (the CPU profile takes 30s) or an unreachable pod (its port-forward
takes up to 10s to time out) doesn't delay the other pods. A pod whose capture
is still running is skipped, and picked up again on the first pass or run
after its capture ends.

Each ProfilingConfig runs at most 4 captures at once
(`maxConcurrentCapturesPerConfig`), threshold and on-demand together, so many
pods crossing a threshold at once don't each open a port-forward through the
API server. Threshold *checks* are never limited. A pod that can't start a
capture because the config is at its limit is retried on the next pass or
run. Pods are visited least recently attempted first, so with more pods than
the limit, every pod gets its turn.

### Invalid Configurations

If a ProfilingConfig fails validation (bucket and region are required), the
reconciler:

- stops the config's monitor (cancelling captures in flight) and untracks its
  pods, so nothing keeps running with the settings the user replaced;
- sets `Ready=False` with reason `InvalidConfig` and the validation error as
  the message, and sets `activePods` to 0;
- emits a `Warning` event (reason `InvalidConfig`) and logs the error, but only
  when the config becomes invalid or its problem changes, not on every
  reconcile;
- returns success once the status is written. Retrying can't fix the spec,
  and the edit that does triggers a new reconcile. If the status write fails,
  the error is returned so it's retried.

When the spec is fixed, monitoring starts again, `Ready` returns to `True`
(reason `Monitoring`), and a `Normal` event (reason `ConfigValid`) confirms the
fix. After a controller restart, the reconciler uses the `Ready` condition to
tell whether a config was already reported invalid. A fixed config's pods
start with fresh cooldowns, unless another config still tracks them.

### Status Counters

- `totalProfiles`: profiles captured (one per profile type that succeeded in
  a capture), whether or not their upload succeeded. Failed profile types
  aren't counted.
- `totalUploads`: profiles actually stored in S3. Each profile is uploaded
  separately, and every profile is attempted even if another fails.
- `lastProfileTime`: when profiles were last captured.

Captures add their counts to a pending delta on the config's monitor. A
single writer per config applies it with an optimistic-concurrency status
update. A write based on a stale cached read, or racing another writer, fails
with a conflict and is retried with a fresh read. The delta is cleared only
after a successful write, so concurrent captures and conflicts can't lose
counts. On shutdown, the controller waits (bounded) for running captures and
the final write. Reconcile updates `activePods` with a patch of that field
alone, so it never overwrites the counters.

### How Profiling Works

`Profiler.CaptureProfiles()` for each target pod:

1. Opens a `kubectl port-forward` via the SPDY protocol (same as `kubectl port-forward` on the CLI but in-process).
2. Picks a random free local port.
3. Sends HTTP GET to `localhost:{port}/debug/pprof/{type}` for each profile type, in order.
4. CPU profile blocks for 30 seconds (that's how pprof works).
5. Returns raw binary data.

Each profile type is tried once. If one fails (say the CPU profile errors on
a struggling pod), the remaining types are still tried, and the profiles that
were captured are kept. If a request fails at the connection level rather
than with an HTTP error, the next type gets a fresh port-forward. When the
pod side reports an error, client-go tears the whole port-forward down, and a
request sent over it meanwhile can hang for 30 seconds. The failure is logged, and the failed type is not
retried. A capture that got at least one profile, and uploaded all of them,
counts as successful and starts the pod's cooldown. If every type fails (or
the port-forward can't be set up), the capture fails: nothing is uploaded or
counted, no cooldown starts, and the pod is retried on the next pass. If an
upload fails, the capture also counts as failed and the pod is retried.

Then `S3Uploader.UploadProfiles()` uploads each file with key:
```
{prefix}/{YYYY-MM-DD}/{service-name}/{YYYYMMDD-HHmmss}-{pod-name}-{type}.pprof
```
The pod name keeps replicas profiled at the same moment from overwriting each
other (see `docs/S3_STRUCTURE.md`).

Each ProfilingConfig has one S3 client, shared by all its captures, so AWS
configuration and credentials are loaded once and connections are reused.
It's built on the first upload and kept on the config's monitor. It's rebuilt
when an edit changes `s3Config.region` or `s3Config.endpoint`, and bucket and
prefix are applied per upload. It's dropped with the monitor when the config
is deleted or becomes invalid. A failed build isn't cached; the next capture
tries again.

### Context Lifetime for Monitoring Goroutines

A key subtlety: `Reconcile(ctx, req)` receives a **per-request context** that controller-runtime cancels as soon as `Reconcile()` returns. Passing that context to the background monitoring goroutines would kill them immediately (and any in-flight `pprof` capture — which can take 30+ seconds for CPU profiles).

The reconciler owns a `baseCtx` (derived from `context.Background()`) that outlives any single reconcile call. All monitoring goroutines are started with `context.WithCancel(r.baseCtx)`. Individual monitors are stopped by calling their cancel function (stored in `activeMonitors`). The entire `baseCtx` is cancelled when the manager shuts down via a `Runnable` registered in `SetupWithManager`.

### Thread Safety

`PodWatcher` uses a `sync.RWMutex` to protect its pod maps — read operations (GetTrackedPods, CanProfile) take a read lock; writes (TrackPod, UpdateLastProfileTime) take an exclusive lock.

The `activeMonitors` map in the reconciler is guarded by `monitorsMu`. Each `configMonitor` guards its current config with its own mutex; the schedule goroutines read it through `current()`.

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

The controller package takes about 35s on a 4-core machine. Most of that is
black-box tests (below) that run in real time, in parallel. They mostly wait
on timers, so raising Go's parallel-test limit (GOMAXPROCS by default) makes
the package finish in about 25s, even on small CI machines:

```bash
go test ./internal/controller -parallel 16
```

Under `-race`, which slows everything several-fold, keep the default: the
real-time tests allow about a second of scheduling delay.

### How the controller tests work

There are two kinds of controller tests:

- **Fake-clock tests** (`monitor_test.go`, `validity_test.go`,
  `s3_client_lifecycle_test.go` and others). They drive the reconciler with a
  fake clock and fake capture, metrics and S3 hooks, so schedules and
  cooldowns are checked exactly and instantly. They use hooks that exist only
  in the current code (`clock`, `captureFn`, `newUploader`, ...).
- **Black-box tests** (`isolation_test.go` and the files listed under
  "Regression tests against the original code" below). They build the
  reconciler with `NewProfilingConfigReconciler` and observe it from the
  outside, through a test API server that serves port-forwards and pprof, a
  fake S3 endpoint, and a recording metrics client. They use only APIs that
  also exist in the original controller, so they compile and run against it.
  They run in real time, with 1–2 second intervals.

`isolation_test.go` holds the shared environment for the black-box tests. Its
`TestMain` sets static fake AWS credentials for the test binary and points the
SDK away from the host's AWS configuration, so uploads never reach real AWS.
It also serializes writes through the fake controller-runtime client. In
v0.18 that client checks resourceVersion and stores an object without a lock,
so a spec edit racing a status write could be lost. The API server compares
and stores atomically.

### Real timings (`BOLOMETER_REAL_TIMING_TESTS`)

The timing regression tests (`timing_regression_test.go`) drive Reconcile at
the controller's requeue cadence. By default they use the same ratios
compressed to a few seconds:

| Test | Default ("scaled") | Real |
|---|---|---|
| Check interval longer than requeue | 2s interval, reconcile every 1s | 60s interval, reconcile every 30s |
| On-demand interval longer than requeue | 2s interval, reconcile every 1s | 35s interval, reconcile every 30s |
| Cooldown survives requeue | 1s checks, 5s cooldown, reconcile every 2s | 10s checks, 60s cooldown, reconcile every 30s |

Set `BOLOMETER_REAL_TIMING_TESTS=1` to also run them with the real values
(about five minutes):

```bash
BOLOMETER_REAL_TIMING_TESTS=1 go test ./internal/controller -run TestTiming -v -timeout 20m
```

### Regression tests against the original code

The black-box regression tests compile against the original code, before the
changes described in this document, so you can watch them fail there. They
need no other test file from the current code.

**Files to copy**

| Into | Files |
|---|---|
| `internal/controller/` | `isolation_test.go` (shared environment, required), `threshold_schedule_test.go`, `ondemand_schedule_test.go`, `capture_limit_test.go`, `status_counters_test.go`, `s3_keys_test.go`, `timing_regression_test.go`, `partial_capture_test.go`, `invalid_config_test.go`, `s3_client_test.go` |
| `internal/profiler/` | `portforward_test.go` |
| `internal/uploader/` | `metadata_test.go`, `object_key_test.go` |

**Steps**

```bash
# From the repository root, check out the original code next to this one.
# Use the last commit before these changes (HEAD while they are uncommitted).
git worktree add /tmp/bolometer-original <original-commit>

cd bolometer
cp internal/controller/{isolation,threshold_schedule,ondemand_schedule,capture_limit,status_counters,s3_keys,timing_regression,partial_capture,invalid_config,s3_client}_test.go \
   /tmp/bolometer-original/bolometer/internal/controller/
cp internal/profiler/portforward_test.go /tmp/bolometer-original/bolometer/internal/profiler/
cp internal/uploader/{metadata,object_key}_test.go /tmp/bolometer-original/bolometer/internal/uploader/

cd /tmp/bolometer-original/bolometer
go test ./internal/controller ./internal/profiler ./internal/uploader -v -timeout 10m \
  -run 'TestIsolation|TestTracking|TestThreshold_|TestOnDemand_|TestCaptureLimit|TestStatusCounters|TestS3Keys|TestTiming_|TestPartialCapture|TestInvalidConfig|TestS3Client|TestCaptureProfiles_|TestUploadProfile_PodWithSlash|TestGenerateKey_Replicas'

# Clean up
cd - && git worktree remove --force /tmp/bolometer-original
```

The run takes one to three minutes on the original code (some tests wait out
hangs). The same command in this repository passes.

**Expected results on the original code**: 23 regression tests fail, and 3
guard tests pass. The guards check behavior that already worked and must
keep working.

| Test | Original | What the original does wrong |
|---|---|---|
| `TestIsolation_ThresholdMonitorOnlyUsesItsOwnPods` | FAIL | checks and captures another config's pods |
| `TestIsolation_OnDemandMonitorOnlyCapturesItsOwnPods` | FAIL | captures another config's pods |
| `TestTracking_DeletedPodIsDropped` | FAIL | keeps tracking and capturing a deleted pod |
| `TestTracking_PodThatStopsMatchingIsDropped` | FAIL | keeps pods that stopped matching; also the control case, because re-tracking wipes cooldowns |
| `TestTracking_DeletedConfigDropsItsPods` | FAIL | keeps a deleted config's pods; another config captures them |
| `TestThreshold_UnresponsivePodsDoNotDelayOtherPodsChecks` | FAIL | one hung port-forward stops all threshold checks |
| `TestOnDemand_UnresponsivePodDoesNotDelayOtherPods` | FAIL | one hung port-forward stops all on-demand captures |
| `TestCaptureLimit_ThresholdCapturesAreCappedPerConfig` | FAIL | captures one pod at a time, then hangs |
| `TestCaptureLimit_AppliesToEachConfigSeparately` | FAIL | same, per config |
| `TestCaptureLimit_SharedByThresholdAndOnDemand` | FAIL | same |
| `TestStatusCounters_MatchCapturedProfilesAndSuccessfulUploads` | FAIL | loses increments on conflicts; counts captures, not profiles |
| `TestS3Keys_ReplicasCapturedTogetherDoNotOverwriteEachOther` | FAIL | replicas overwrite each other's S3 objects |
| `TestTiming_CheckIntervalLongerThanRequeueStillFires` | FAIL | never checks (each reconcile restarts the timer) |
| `TestTiming_OnDemandIntervalLongerThanRequeueStillCaptures` | FAIL | never captures |
| `TestTiming_CooldownSurvivesRequeue` | FAIL | captures again within the cooldown |
| `TestTiming_SpecEditReachesRunningMonitor` | pass | guard |
| `TestPartialCapture_CPUFailsHeapAndGoroutineStillUploaded` | FAIL | one failed profile type discards the others |
| `TestPartialCapture_LostConnectionDoesNotLoseRemainingTypes` | FAIL | same, for a lost connection |
| `TestPartialCapture_AllTypesFailIsAFailedCapture` | pass | guard |
| `TestInvalidConfig_BadEditStopsMonitorAndIsReportedUntilFixed` | FAIL | keeps profiling with the replaced settings; no condition or event |
| `TestS3Client_CapturesOfAConfigShareOneClient` | FAIL | builds an S3 client (and connection) per capture |
| `TestS3Client_EditsToS3SettingsReachUploads` | pass | guard |
| `TestCaptureProfiles_PortForwardRejected` (profiler) | FAIL | hangs on a rejected port-forward |
| `TestCaptureProfiles_ContextCancelledDuringPortForwardSetup` (profiler) | FAIL | ignores cancellation while setting up |
| `TestUploadProfile_PodWithSlashInLabelKey` (uploader) | FAIL | upload fails for pods with `app.kubernetes.io/*` labels |
| `TestGenerateKey_ReplicasCapturedTogetherGetDistinctKeys` (uploader) | FAIL | object keys lack the pod name |

Some controller tests also exist in fake-clock form in `monitor_test.go`. Those
are exact but use hooks, so they only build against the current code.

### What's Tested

| Package | Tests |
|---|---|
| `controller` | Reconcile lifecycle; config validation, the `Ready` condition and events; per-config pod tracking and untracking; monitor schedules, cooldowns and spec edits (fake clock); background captures, the capture limit and its rotation; partial captures; status counters under conflicts and at shutdown; the per-config S3 client; end-to-end captures through port-forward and S3 (black box) |
| `metrics` | Threshold comparison, CPU/memory percentage calculation with edge cases |
| `profiler` | pprof port resolution (annotation parsing), profile endpoint URLs, port-forward failure and cancellation |
| `uploader` | S3 key generation (including the pod name), service name extraction from labels/owner refs, metadata header names |

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

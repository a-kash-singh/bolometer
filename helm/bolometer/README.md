# Bolometer Helm Chart

Kubernetes operator that automatically captures Go pprof profiles when applications exceed resource thresholds or on demand, uploading them to S3.

## Quick Install

### Local testing (kind + MinIO — no AWS account needed)

```bash
# Full automated e2e test (builds operator, spins up kind cluster, validates captures)
./e2e-local.sh

# Or install manually into an existing kind cluster
helm upgrade --install bolometer ./helm/bolometer \
  -n bolometer-system --create-namespace \
  -f ./helm/bolometer/values-minio.yaml \
  --set image.tag=latest
```

### Production (EKS + IRSA)

```bash
# 1. Create the IAM role (one-time)
CLUSTER_NAME=my-cluster AWS_REGION=us-west-2 S3_BUCKET=my-pprof-bucket \
  ./aws/irsa-setup.sh

# 2. Install
helm upgrade --install bolometer ./helm/bolometer \
  -n bolometer-system --create-namespace \
  -f ./helm/bolometer/values-aws.yaml \
  --set aws.irsa.roleArn=arn:aws:iam::ACCOUNT:role/bolometer-role \
  --set aws.region=us-west-2 \
  --set image.repository=ACCOUNT.dkr.ecr.REGION.amazonaws.com/bolometer \
  --set image.tag=0.1.0
```

## Configuration Reference

### AWS Credentials

| Value | Default | Description |
|---|---|---|
| `aws.region` | `us-east-1` | AWS region injected as `AWS_REGION` |
| `aws.irsa.enabled` | `false` | Add OIDC annotation to ServiceAccount for IRSA |
| `aws.irsa.roleArn` | `""` | `arn:aws:iam::ACCOUNT:role/bolometer-role` |
| `aws.staticCredentials.enabled` | `false` | Inject `AWS_ACCESS_KEY_ID` / `AWS_SECRET_ACCESS_KEY` from a Secret |
| `aws.staticCredentials.accessKeyId` | `""` | Plain-text key ID (creates a Secret) |
| `aws.staticCredentials.secretAccessKey` | `""` | Plain-text secret (creates a Secret) |
| `aws.staticCredentials.existingSecret` | `""` | Name of a pre-created Secret (skips Secret creation) |

### Image & Deployment

| Value | Default | Description |
|---|---|---|
| `image.repository` | `bolometer` | Image repository |
| `image.tag` | `latest` | Image tag |
| `image.pullPolicy` | `IfNotPresent` | Pull policy |
| `replicaCount` | `1` | Operator replicas |
| `leaderElection.enabled` | `true` | Disable for single-node / testing |

### Optional ProfilingConfig

Set `profilingConfig.create: true` to deploy a `ProfilingConfig` CR as part of this Helm release (useful for GitOps):

```yaml
profilingConfig:
  create: true
  name: my-app
  namespace: production
  selector:
    namespace: production
    labelSelector:
      app: my-go-app
  thresholds:
    cpuThresholdPercent: 80
    cooldownSeconds: 300
  s3:
    bucket: my-pprof-bucket
    region: us-west-2
  profileTypes: [heap, cpu, goroutine]
```

## Usage

After install, create a `ProfilingConfig` and annotate your pods:

```yaml
# ProfilingConfig — one per app or namespace
apiVersion: bolometer.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: my-app-profiling
  namespace: default
spec:
  selector:
    namespace: default
    labelSelector:
      app: my-app
  thresholds:
    cpuThresholdPercent: 80
    memoryThresholdPercent: 90
    checkIntervalSeconds: 30
    cooldownSeconds: 300
  s3Config:
    bucket: my-pprof-bucket
    prefix: profiles
    region: us-west-2
  profileTypes: [heap, cpu, goroutine, mutex]
```

```yaml
# Pod annotations (add to your Deployment's pod template)
metadata:
  annotations:
    bolometer.io/enabled: "true"
    bolometer.io/port: "6060"   # default 6060; omit if using default
```

Your Go app must also expose pprof:

```go
import _ "net/http/pprof"
go http.ListenAndServe(":6060", nil)
```

## Uninstall

```bash
helm uninstall bolometer -n bolometer-system
```

## More

- [DEVELOPER.md](../../DEVELOPER.md) — architecture, context lifetime, testing guide
- [aws/irsa-setup.sh](../../aws/irsa-setup.sh) — IRSA IAM role creation
- [aws/iam-policy.json](../../aws/iam-policy.json) — minimal S3 IAM policy

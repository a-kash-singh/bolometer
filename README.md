# Bolometer

> *Measuring the heat of your applications*

Bolometer is a Kubernetes operator that captures Go pprof profiles when your applications run hot. Like a scientific bolometer that detects radiant heat, this operator measures your application's resource intensity and captures performance profiles when things heat up.

## Features

- **Threshold-based Profiling**: Automatically captures profiles when CPU or memory usage exceeds configured thresholds
- **On-demand Profiling**: Continuous profiling at configurable intervals (30-40 seconds)
- **Multiple Profile Types**: Supports heap, CPU, goroutine, and mutex profiles
- **S3 Integration**: Uploads profiles to S3 with structured naming and metadata
- **IRSA Support**: Uses IAM Roles for Service Accounts for secure AWS authentication
- **Annotation-based**: Target pods using `profiling.io/enabled: "true"` annotation
- **Cooldown Period**: Prevents excessive profiling with configurable cooldown
- **Label Selection**: Filter target pods by namespace and labels

## Architecture

The operator consists of:
- **ProfilingConfig CRD**: Defines profiling configuration
- **Controller**: Reconciles ProfilingConfig resources and manages profiling lifecycle
- **Pod Watcher**: Tracks pods with profiling annotation
- **Metrics Collector**: Fetches pod metrics from metrics-server API
- **Profiler**: Captures pprof dumps via port-forward
- **S3 Uploader**: Uploads profiles to S3 with metadata

## Prerequisites

- Kubernetes 1.30+
- Go 1.23+ (for development)
- metrics-server installed in the cluster
- S3 bucket for profile storage
- IAM role with S3 write permissions (for IRSA)

## Installation

### Quick Install (Recommended)

Use the automated installation script with validation:

```bash
./install.sh
```

The script will:
- ✅ Validate prerequisites (kubectl, cluster access, metrics-server)
- 📝 Prompt for configuration (S3 bucket, region, IAM role)
- 🚀 Install the operator components
- ✅ Validate the installation

For detailed installation options and troubleshooting, see [Installation Guide](docs/INSTALLATION.md).

### Using Helm

1. Install the operator:

```bash
helm install bolometer helm/bolometer \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::ACCOUNT_ID:role/bolometer-role" \
  --namespace bolometer-system \
  --create-namespace
```

2. Create a ProfilingConfig:

```bash
kubectl apply -f config/samples/profiling_v1alpha1_profilingconfig.yaml
```

### Using kubectl

1. Install CRDs and RBAC:

```bash
kubectl apply -f config/crd/
kubectl apply -f config/rbac/
```

2. Deploy the operator:

```bash
kubectl apply -f config/manager/
```

## Configuration

### IAM Role for IRSA

Create an IAM role with the following policy:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Action": [
        "s3:PutObject",
        "s3:PutObjectAcl"
      ],
      "Resource": "arn:aws:s3:::YOUR_BUCKET/profiles/*"
    }
  ]
}
```

Add trust relationship for the service account:

```json
{
  "Version": "2012-10-17",
  "Statement": [
    {
      "Effect": "Allow",
      "Principal": {
        "Federated": "arn:aws:iam::ACCOUNT_ID:oidc-provider/oidc.eks.REGION.amazonaws.com/id/OIDC_ID"
      },
      "Action": "sts:AssumeRoleWithWebIdentity",
      "Condition": {
        "StringEquals": {
          "oidc.eks.REGION.amazonaws.com/id/OIDC_ID:sub": "system:serviceaccount:bolometer-system:bolometer"
        }
      }
    }
  ]
}
```

### Target Pod Setup

Annotate your pods and expose pprof endpoints:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-go-app
  annotations:
    profiling.io/enabled: "true"
    profiling.io/port: "6060"  # Optional, defaults to 6060
spec:
  containers:
  - name: app
    image: my-go-app:latest
```

In your Go application:

```go
package main

import (
    "log"
    "net/http"
    _ "net/http/pprof"
)

func main() {
    // Start pprof server
    go func() {
        log.Println(http.ListenAndServe(":6060", nil))
    }()

    // Your application code
    // ...
}
```

### ProfilingConfig Resource

Create a ProfilingConfig to define profiling behavior:

```yaml
apiVersion: profiling.io/v1alpha1
kind: ProfilingConfig
metadata:
  name: my-profiling-config
  namespace: default
spec:
  # Select pods to profile
  selector:
    namespace: default
    labelSelector:
      app: my-go-app
  
  # Threshold configuration
  thresholds:
    cpuThresholdPercent: 80        # Trigger when CPU > 80%
    memoryThresholdPercent: 90     # Trigger when Memory > 90%
    checkIntervalSeconds: 30       # Check every 30 seconds
    cooldownSeconds: 300           # Wait 5 minutes between profiles
  
  # Optional: On-demand profiling
  onDemand:
    enabled: true
    intervalSeconds: 35            # Profile every 35 seconds
  
  # S3 configuration
  s3Config:
    bucket: my-profiling-bucket
    prefix: profiles
    region: us-west-2
  
  # Profile types to capture
  profileTypes:
  - heap
  - cpu
  - goroutine
  - mutex
```

## Operating Modes

### Abnormality Detection Mode (Default)

Monitors pod metrics and captures profiles when thresholds are exceeded:
- Checks metrics every `checkIntervalSeconds`
- Triggers profiling when CPU or memory exceeds threshold
- Applies cooldown period to avoid excessive captures
- Uploads with reason: "threshold-exceeded"

### On-Demand Mode

Continuously captures profiles at regular intervals:
- Captures profiles every `intervalSeconds` (30-40 seconds)
- Independent of threshold checks
- Uploads with reason: "on-demand"
- Can be enabled alongside threshold monitoring

## Profile Storage

Profiles are uploaded to S3 with structured naming organized by date and service:

```
s3://{bucket}/{prefix}/{date}/{service-name}/{timestamp}-{profile-type}.pprof
```

Example:
```
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-heap.pprof
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-cpu.pprof
s3://my-bucket/profiles/2024-01-15/my-app/20240115-143000-heap.pprof
```

Service name is extracted from (in order of preference):
1. `app.kubernetes.io/name` label (recommended)
2. `app` label (common convention)
3. `k8s-app` label
4. Owner reference name (Deployment, StatefulSet, etc.)
5. Pod name prefix (fallback)

Metadata tags include:
- pod-name
- pod-namespace
- profile-type
- reason (threshold-exceeded or on-demand)
- timestamp
- pod labels

## Development

### Build

```bash
make build
```

### Run locally

```bash
make run
```

### Docker build

```bash
make docker-build IMG=your-registry/bolometer:tag
```

### Deploy

```bash
make deploy
```

## Monitoring

The operator exposes Prometheus metrics on port 8080:
- `profiling_captures_total`: Total number of profile captures
- `profiling_uploads_total`: Total number of successful uploads
- `profiling_errors_total`: Total number of errors
- `profiling_threshold_violations_total`: Total threshold violations

Health checks:
- Liveness: `http://localhost:8081/healthz`
- Readiness: `http://localhost:8081/readyz`

## Troubleshooting

### Profiles not being captured

1. Check pod has the annotation:
   ```bash
   kubectl get pod <pod-name> -o jsonpath='{.metadata.annotations}'
   ```

2. Verify pprof endpoint is accessible:
   ```bash
   kubectl port-forward pod/<pod-name> 6060:6060
   curl http://localhost:6060/debug/pprof/
   ```

3. Check ProfilingConfig status:
   ```bash
   kubectl get profilingconfig <name> -o yaml
   ```

### S3 upload failures

1. Verify IRSA annotation on service account:
   ```bash
   kubectl get sa bolometer -n bolometer-system -o yaml
   ```

2. Check operator logs:
   ```bash
   kubectl logs -n bolometer-system -l app=bolometer
   ```

3. Verify IAM role has S3 permissions

### Metrics not available

1. Check metrics-server is running:
   ```bash
   kubectl get deployment metrics-server -n kube-system
   ```

2. Test metrics API:
   ```bash
   kubectl top pods
   ```

## Examples

See `config/samples/` for example configurations:
- `profiling_v1alpha1_profilingconfig.yaml`: Basic threshold-based profiling
- `profiling_v1alpha1_ondemand.yaml`: On-demand continuous profiling

## Contributing

Contributions are welcome! Please ensure:
- Code passes `make fmt` and `make vet`
- Tests are included for new features
- Documentation is updated

## License

Apache 2.0

## Support

For issues and questions, please open a GitHub issue.

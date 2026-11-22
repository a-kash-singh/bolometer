# Bolometer

> *Measuring the heat of your applications*

Bolometer is a Kubernetes operator that captures Go pprof profiles when your applications run hot. Like a scientific bolometer that detects radiant heat, this operator measures your application's resource intensity and captures performance profiles when things heat up.

## Features

- **Threshold-based Profiling**: Automatically captures profiles when CPU or memory usage exceeds configured thresholds
- **On-demand Profiling**: Continuous profiling at configurable intervals (30-60 seconds)
- **Multiple Profile Types**: Supports heap, CPU, goroutine, and mutex profiles
- **S3 Integration**: Uploads profiles to S3 with structured naming and metadata
- **IRSA Support**: Uses IAM Roles for Service Accounts for secure AWS authentication
- **Annotation-based**: Target pods using `bolometer.io/enabled: "true"` annotation
- **Cooldown Period**: Prevents excessive profiling with configurable cooldown
- **Label Selection**: Filter target pods by namespace and labels

## Project Structure

```
bolometer/
├── api/v1alpha1/                           # API definitions
│   ├── groupversion_info.go                # API group version info
│   ├── profilingconfig_types.go            # ProfilingConfig CRD types
│   └── zz_generated.deepcopy.go            # Generated deep copy methods
├── cmd/
│   └── main.go                             # Operator entry point
├── config/                                 # Kubernetes manifests
│   ├── crd/
│   │   └── bolometer.io_profilingconfigs.yaml  # CRD definition
│   ├── manager/
│   │   ├── deployment.yaml                 # Operator deployment
│   │   └── kustomization.yaml              # Kustomize config
│   ├── rbac/                               # RBAC configurations
│   │   ├── role.yaml                       # ClusterRole
│   │   ├── role_binding.yaml               # ClusterRoleBinding
│   │   └── service_account.yaml            # ServiceAccount with IRSA
│   └── samples/                            # Example ProfilingConfigs
│       ├── profiling_v1alpha1_profilingconfig.yaml
│       └── profiling_v1alpha1_ondemand.yaml
├── docs/
│   └── IRSA_SETUP.md                       # IRSA setup guide
├── examples/
│   ├── sample-app/                         # Sample Go app with pprof
│   │   ├── Dockerfile
│   │   └── main.go
│   └── target-app.yaml                     # Sample deployment
├── helm/bolometer/                         # Helm chart
│   ├── Chart.yaml
│   ├── values.yaml
│   └── templates/
│       ├── _helpers.tpl
│       ├── crd.yaml
│       ├── deployment.yaml
│       ├── namespace.yaml
│       ├── rbac.yaml
│       ├── service.yaml
│       └── serviceaccount.yaml
├── internal/
│   ├── controller/                         # Controller logic
│   │   ├── pod_watcher.go                  # Pod tracking
│   │   └── profilingconfig_controller.go   # Main reconciler
│   ├── metrics/                            # Metrics collection
│   │   └── collector.go                    # Metrics-server client
│   ├── profiler/                           # Profile capture
│   │   └── profiler.go                     # pprof client
│   └── uploader/                           # S3 upload
│       └── s3.go                           # S3 client
├── Dockerfile                              # Operator container image
├── Makefile                                # Build automation
├── README.md                               # Main documentation
├── go.mod                                  # Go dependencies
└── go.sum                                  # Go checksums
```

## Architecture

The operator consists of:

### ProfilingConfig CRD

Defines profiling behavior for target pods.

Key fields:
- `selector`: Pod selection criteria (namespace, labels)
- `thresholds`: Resource thresholds (CPU, memory percentages)
- `onDemand`: Continuous profiling configuration
- `s3Config`: S3 bucket and region settings
- `profileTypes`: Types of profiles to capture

### Controller

**ProfilingConfigReconciler** - Main reconciliation loop

Responsibilities:
- Watch ProfilingConfig resources
- Discover and track annotated pods
- Manage monitoring goroutines
- Coordinate profiling operations
- Update status metrics

### Pod Watcher

Tracks pods with profiling enabled.

Features:
- Lists pods by annotation and labels
- Maintains active pod tracking
- Manages cooldown periods
- Thread-safe pod map

### Metrics Collector

Fetches pod metrics from metrics-server.

Capabilities:
- Query PodMetrics API
- Calculate CPU/memory usage percentages
- Compare against thresholds
- Detect abnormalities

### Profiler

Captures pprof profiles from target pods.

Features:
- Port-forward to pod's pprof endpoint
- Capture multiple profile types (heap, CPU, goroutine, mutex)
- Configurable pprof port via annotation
- Timeout and error handling

### S3 Uploader

Uploads profiles to S3.

Features:
- AWS SDK v2 integration
- IRSA/IAM role authentication
- Structured S3 key naming
- Metadata tagging
- Retry logic

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
    bolometer.io/enabled: "true"
    bolometer.io/port: "6060"  # Optional, defaults to 6060
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

### Key Annotations

- `bolometer.io/enabled: "true"` - Enable profiling for pod
- `bolometer.io/port: "6060"` - Custom pprof port (optional)

### ProfilingConfig Resource

Create a ProfilingConfig to define profiling behavior:

```yaml
apiVersion: bolometer.io/v1alpha1
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

### Helm Values

Key configurations:
- `serviceAccount.annotations` - IRSA role ARN
- `defaultConfig.s3.bucket` - S3 bucket name
- `defaultConfig.s3.region` - AWS region
- `defaultConfig.thresholds.*` - Default thresholds
- `resources.*` - Operator resource limits

## Operating Modes

### Threshold-Based Profiling (Default)

Monitors pod metrics and captures profiles when thresholds are exceeded:
- Monitor pod metrics every `checkIntervalSeconds`
- When CPU or memory exceeds threshold:
  - Capture all configured profile types (a type that fails doesn't stop the others)
  - Upload to S3 with the threshold that was exceeded as the reason
    (for example `CPU usage 95.00% exceeds threshold 80%`)
  - Apply cooldown period

### On-Demand Mode

Continuously captures profiles at regular intervals:
- Captures profiles every `intervalSeconds` (30-60 seconds)
- Upload immediately after capture
- Independent of threshold checks
- Uploads with reason: "on-demand"
- Can run alongside threshold monitoring

## Profile Storage

Profiles are uploaded to S3 with structured naming organized by date and service:

```
s3://{bucket}/{prefix}/{date}/{service-name}/{timestamp}-{pod-name}-{profile-type}.pprof
```

Where:
- `date`: YYYY-MM-DD format (e.g., 2024-01-15)
- `service-name`: Extracted from pod labels or metadata
- `timestamp`: YYYYMMDD-HHmmss format
- `pod-name`: The pod the profile was captured from, so replicas profiled at
  the same moment don't overwrite each other

Example:
```
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-my-app-7d8f9c5b6d-xyz456-heap.pprof
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-my-app-7d8f9c5b6d-xyz456-cpu.pprof
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-my-app-7d8f9c5b6d-abc123-heap.pprof
```

See [docs/S3_STRUCTURE.md](docs/S3_STRUCTURE.md) for details, including how
to parse filenames.

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
- reason (the threshold that was exceeded, or `on-demand`)
- timestamp
- pod labels

## RBAC Permissions

The operator requires:
- Read pods (get, list, watch)
- Create port-forward (pods/portforward)
- Read metrics (metrics.k8s.io)
- Manage ProfilingConfigs (all verbs)
- Create events

## Dependencies

### Go Modules

- `sigs.k8s.io/controller-runtime` - Controller framework
- `k8s.io/client-go` - Kubernetes client
- `k8s.io/metrics` - Metrics API client
- `github.com/aws/aws-sdk-go-v2` - AWS SDK
- `k8s.io/api` - Kubernetes API types

### Kubernetes Resources

- metrics-server - Required for pod metrics
- IRSA/IAM roles - Required for S3 access

## Development

### Local Development

```bash
# Install dependencies
make deps

# Format and vet
make fmt vet

# Build
make build

# Run locally (requires kubeconfig)
make run
```

### Docker Build

```bash
make docker-build IMG=your-registry/bolometer:tag
make docker-push IMG=your-registry/bolometer:tag
```

### Deploy to Cluster

```bash
# Using kubectl
make install  # Install CRDs
make deploy   # Deploy operator

# Using Helm
make helm-install
```

## Testing

### Unit Tests

```bash
make test
```

### Integration Testing

1. Deploy operator
2. Deploy sample application with pprof
3. Create ProfilingConfig
4. Generate load
5. Verify profiles in S3

## Monitoring

The operator serves controller-runtime's standard Prometheus metrics on port
8080 (`/metrics`): reconcile counts, errors and durations, work queue depth,
and API client requests. It doesn't export profiling-specific metrics.
Profiling activity is reported per ProfilingConfig instead:

- Status: `activePods`, `totalProfiles` (profiles captured), `totalUploads`
  (profiles stored in S3), `lastProfileTime`, and the `Ready` condition
- Events: `InvalidConfig` (Warning) and `ConfigValid` (Normal)

```bash
kubectl get profilingconfig -A     # active pods, profiles, uploads
kubectl describe profilingconfig <name> -n <namespace>
```

Health checks:
- Liveness: `http://localhost:8081/healthz`
- Readiness: `http://localhost:8081/readyz`

### Logging

Structured logging with:
- Info: Normal operations
- Error: Failures and retries
- Debug: Detailed diagnostics

## Troubleshooting

### Common Issues

1. **Profiles not captured**: Check pprof endpoint, annotations
2. **S3 upload fails**: Verify IRSA, IAM permissions
3. **Metrics unavailable**: Check metrics-server
4. **High CPU usage**: Increase intervals, reduce profile types

### ProfilingConfig is invalid

If a ProfilingConfig is invalid (for example, `s3Config.bucket` or
`s3Config.region` is empty), the operator stops monitoring it. It doesn't keep
using the previous settings. The problem is reported in the `Ready` condition
and as a `Warning` event with reason `InvalidConfig`:

```bash
kubectl describe profilingconfig <name> -n <namespace>
kubectl get profilingconfig <name> -n <namespace> \
  -o jsonpath='{.status.conditions[?(@.type=="Ready")]}'
```

Once the spec is fixed, monitoring resumes, `Ready` returns to `True`, and a
`Normal` event with reason `ConfigValid` confirms the fix.

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

### Debug Commands

```bash
# Check operator logs
kubectl logs -n bolometer-system -l app=bolometer -f

# Check ProfilingConfig status
kubectl get profilingconfig -A
kubectl describe profilingconfig <name> -n <namespace>

# Check metrics
kubectl top pods

# Test pprof endpoint
kubectl port-forward <pod> 6060:6060
curl http://localhost:6060/debug/pprof/
```

## Security Considerations

1. **IRSA**: Use IAM roles instead of static credentials
2. **RBAC**: Least privilege permissions
3. **Pod Security**: Non-root user, dropped capabilities
4. **S3 Encryption**: Enable bucket encryption
5. **Network Policies**: Restrict operator egress

## Performance Considerations

1. **Cooldown Period**: Prevent excessive profiling
2. **Check Interval**: Balance between responsiveness and overhead
3. **Profile Types**: More types = more overhead
4. **Concurrent Profiling**: One pod at a time per config
5. **Resource Limits**: Set appropriate operator limits

## Cost Optimization

1. **S3 Lifecycle**: Auto-delete old profiles
2. **Intelligent Tiering**: Move to cheaper storage
3. **Profile Frequency**: Adjust intervals based on needs
4. **Selective Profiling**: Profile only critical pods

## Examples

See `config/samples/` for example configurations:
- `profiling_v1alpha1_profilingconfig.yaml`: Basic threshold-based profiling
- `profiling_v1alpha1_ondemand.yaml`: On-demand continuous profiling

## Future Enhancements

Potential improvements:
1. Support for other languages (Python, Java, Node.js)
2. Profile comparison and analysis
3. Alerting integration
4. Web UI for profile visualization
5. Automatic anomaly detection with ML
6. Profile streaming instead of batch upload
7. Support for other storage backends (GCS, Azure Blob)
8. CPU flame graph generation
9. Memory leak detection
10. Integration with observability platforms

## Contributing

Contributions are welcome! Please ensure:
- Code passes `make fmt` and `make vet`
- Tests are included for new features
- Documentation is updated
- Run `make fmt vet test` before submitting
- Sign commits

## License

Apache 2.0

## Support

For issues and questions, please open a GitHub issue.

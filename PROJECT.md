# Profiling Operator - Project Overview

## Project Structure

```
k8s-operator/
├── api/v1alpha1/                           # API definitions
│   ├── groupversion_info.go                # API group version info
│   ├── profilingconfig_types.go            # ProfilingConfig CRD types
│   └── zz_generated.deepcopy.go            # Generated deep copy methods
├── cmd/
│   └── main.go                             # Operator entry point
├── config/                                 # Kubernetes manifests
│   ├── crd/
│   │   └── profiling.io_profilingconfigs.yaml  # CRD definition
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
├── helm/profiling-operator/                # Helm chart
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
├── QUICKSTART.md                           # Quick start guide
├── README.md                               # Main documentation
├── go.mod                                  # Go dependencies
└── go.sum                                  # Go checksums

```

## Architecture Components

### 1. Custom Resource Definition (CRD)

**ProfilingConfig** - Defines profiling behavior for target pods

Key fields:
- `selector`: Pod selection criteria (namespace, labels)
- `thresholds`: Resource thresholds (CPU, memory percentages)
- `onDemand`: Continuous profiling configuration
- `s3Config`: S3 bucket and region settings
- `profileTypes`: Types of profiles to capture

### 2. Controller

**ProfilingConfigReconciler** - Main reconciliation loop

Responsibilities:
- Watch ProfilingConfig resources
- Discover and track annotated pods
- Manage monitoring goroutines
- Coordinate profiling operations
- Update status metrics

### 3. Pod Watcher

**PodWatcher** - Tracks pods with profiling enabled

Features:
- Lists pods by annotation and labels
- Maintains active pod tracking
- Manages cooldown periods
- Thread-safe pod map

### 4. Metrics Collector

**Collector** - Fetches pod metrics from metrics-server

Capabilities:
- Query PodMetrics API
- Calculate CPU/memory usage percentages
- Compare against thresholds
- Detect abnormalities

### 5. Profiler

**Profiler** - Captures pprof profiles from target pods

Features:
- Port-forward to pod's pprof endpoint
- Capture multiple profile types (heap, CPU, goroutine, mutex)
- Configurable pprof port via annotation
- Timeout and error handling

### 6. S3 Uploader

**S3Uploader** - Uploads profiles to S3

Features:
- AWS SDK v2 integration
- IRSA/IAM role authentication
- Structured S3 key naming
- Metadata tagging
- Retry logic

## Operating Modes

### Threshold-Based Profiling (Default)

1. Monitor pod metrics every `checkIntervalSeconds`
2. When CPU or memory exceeds threshold:
   - Capture all configured profile types
   - Upload to S3 with reason: "threshold-exceeded"
   - Apply cooldown period

### On-Demand Profiling

1. Continuously capture profiles every `intervalSeconds`
2. Upload immediately after capture
3. Independent of threshold checks
4. Can run alongside threshold monitoring

## Key Annotations

- `profiling.io/enabled: "true"` - Enable profiling for pod
- `profiling.io/port: "6060"` - Custom pprof port (optional)

## S3 Key Structure

```
s3://{bucket}/{prefix}/{date}/{service-name}/{timestamp}-{profile-type}.pprof
```

Where:
- `date`: YYYY-MM-DD format (e.g., 2024-01-15)
- `service-name`: Extracted from pod labels or metadata
- `timestamp`: YYYYMMDDHHmmss format

Example:
```
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-heap.pprof
s3://my-bucket/profiles/2024-01-15/my-app/20240115-120000-cpu.pprof
```

Service name extraction priority:
1. `app.kubernetes.io/name` label (recommended)
2. `app` label (common convention)
3. `k8s-app` label
4. Owner reference name (Deployment, StatefulSet)
5. Pod name prefix (fallback)

## RBAC Permissions

The operator requires:
- Read pods (get, list, watch)
- Create port-forward (pods/portforward)
- Read metrics (metrics.k8s.io)
- Manage ProfilingConfigs (all verbs)
- Create events

## Configuration

### Helm Values

Key configurations:
- `serviceAccount.annotations` - IRSA role ARN
- `defaultConfig.s3.bucket` - S3 bucket name
- `defaultConfig.s3.region` - AWS region
- `defaultConfig.thresholds.*` - Default thresholds
- `resources.*` - Operator resource limits

### ProfilingConfig

Per-namespace or cluster-wide configurations:
- Different thresholds per workload
- Different S3 prefixes for organization
- Enable/disable on-demand profiling
- Select specific profile types

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

## Build and Deploy

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
make docker-build IMG=your-registry/profiling-operator:tag
make docker-push IMG=your-registry/profiling-operator:tag
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

### Operator Metrics

Exposed on port 8080:
- `profiling_captures_total` - Total profile captures
- `profiling_uploads_total` - Total S3 uploads
- `profiling_errors_total` - Total errors
- `profiling_threshold_violations_total` - Threshold violations

### Health Checks

- Liveness: `/healthz` on port 8081
- Readiness: `/readyz` on port 8081

### Logging

Structured logging with:
- Info: Normal operations
- Error: Failures and retries
- Debug: Detailed diagnostics

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

## Troubleshooting

### Common Issues

1. **Profiles not captured**: Check pprof endpoint, annotations
2. **S3 upload fails**: Verify IRSA, IAM permissions
3. **Metrics unavailable**: Check metrics-server
4. **High CPU usage**: Increase intervals, reduce profile types

### Debug Commands

```bash
# Check operator logs
kubectl logs -n profiling-system -l app=profiling-operator -f

# Check ProfilingConfig status
kubectl get profilingconfig -A
kubectl describe profilingconfig <name> -n <namespace>

# Check metrics
kubectl top pods

# Test pprof endpoint
kubectl port-forward <pod> 6060:6060
curl http://localhost:6060/debug/pprof/
```

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

Guidelines:
1. Follow Go best practices
2. Add tests for new features
3. Update documentation
4. Run `make fmt vet test` before submitting
5. Sign commits

## License

Apache 2.0


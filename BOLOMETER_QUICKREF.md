# Bolometer Quick Reference

## Name & Pronunciation

**Bolometer**
Pronunciation: **boh-LOM-uh-ter** (emphasis on second syllable)
Meaning: *A scientific instrument for measuring radiant heat*

## Tagline

> **Measuring the heat of your applications**

## Quick Commands

### Installation
```bash
./install.sh
```

### Build & Test
```bash
make build          # Build binary
make test           # Run tests
make test-coverage  # Show coverage
```

### Docker
```bash
make docker-build IMG=bolometer:latest
kind load docker-image bolometer:latest
```

### Deploy
```bash
# Helm
helm install bolometer helm/bolometer -n bolometer-system --create-namespace

# Kubectl
kubectl apply -f config/
```

## Key URLs (Once Migrated)

```
GitHub:     github.com/bolometer-io/bolometer
Go Module:  github.com/bolometer-io/bolometer
Docker:     bolometer/operator:latest
GHCR:       ghcr.io/bolometer-io/bolometer:latest
Helm:       helm repo add bolometer https://bolometer-io.github.io/charts
```

## Naming Conventions

| Context | Format | Example |
|---------|--------|---------|
| Project name | Bolometer | "Bolometer operator" |
| Code/URLs | bolometer (lowercase) | github.com/bolometer-io/bolometer |
| Docker image | bolometer | bolometer:latest |
| Namespace | bolometer-system | kubectl get pods -n bolometer-system |
| Helm chart | bolometer | helm install bolometer |
| Labels | bolometer | app.kubernetes.io/name: bolometer |

## What Stays the Same

✅ **CRD Domain**: `profiling.io/v1alpha1`
✅ **Annotations**: `profiling.io/enabled: "true"`
✅ **Kind**: `ProfilingConfig`
✅ **All existing configs work** - Zero breaking changes!

## What Changes

📝 **Project Name**: Profiling Operator → Bolometer
📝 **Repository**: github.com/bolometer-io/bolometer
📝 **Images**: bolometer:latest
📝 **Namespace**: profiling-system → bolometer-system (optional)

## Core Features

- ⚡ **Threshold-based profiling** - Capture when CPU/memory exceeds limits
- 📊 **Multiple profile types** - heap, CPU, goroutine, mutex
- 🪣 **S3 integration** - Automatic upload with structured naming
- 🔐 **IRSA support** - Secure AWS authentication
- 🎯 **Annotation-based** - Simple pod targeting
- ⏱️ **Cooldown periods** - Prevent excessive profiling
- 🧪 **Well-tested** - 66.5% test coverage with 39 tests

## Quick Example

```yaml
apiVersion: profiling.io/v1alpha1
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
    bucket: my-profiling-bucket
    prefix: profiles
    region: us-west-2
  profileTypes:
    - heap
    - cpu
```

## Annotate Your Pods

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-go-app
  annotations:
    profiling.io/enabled: "true"
    profiling.io/port: "6060"
spec:
  containers:
  - name: app
    image: my-go-app:latest
```

## Architecture

```
┌─────────────────┐
│ ProfilingConfig │ (CRD)
└────────┬────────┘
         │
    ┌────▼────────┐
    │ Controller  │ (Reconciles configs)
    └────┬────────┘
         │
    ┌────▼─────────┐
    │ Pod Watcher  │ (Tracks annotated pods)
    └────┬─────────┘
         │
    ┌────▼───────────────┐
    │ Metrics Collector  │ (Checks thresholds)
    └────┬───────────────┘
         │
    ┌────▼─────────┐
    │   Profiler   │ (Captures pprof via port-forward)
    └────┬─────────┘
         │
    ┌────▼──────────┐
    │ S3 Uploader   │ (Uploads profiles)
    └───────────────┘
```

## File Structure

```
bolometer/
├── api/v1alpha1/              # CRD definitions
├── cmd/                       # Main entry point
├── config/                    # K8s manifests
│   ├── crd/                   # CRD YAML
│   ├── rbac/                  # RBAC resources
│   ├── manager/               # Deployment
│   └── samples/               # Example configs
├── docs/                      # Documentation
├── helm/bolometer/            # Helm chart
├── internal/
│   ├── controller/            # Reconciliation logic
│   ├── profiler/              # Profile capture
│   ├── metrics/               # Metrics collection
│   └── uploader/              # S3 upload
├── install.sh                 # Installation script
├── Makefile                   # Build targets
└── README.md                  # Project readme
```

## Common Tasks

### Check Installation
```bash
kubectl get pods -n bolometer-system
kubectl get profilingconfigs -A
kubectl logs -n bolometer-system -l app=bolometer
```

### View Profiles in S3
```bash
aws s3 ls s3://my-bucket/profiles/ --recursive
```

### Debug
```bash
kubectl describe profilingconfig my-config
kubectl get events -n bolometer-system --sort-by='.lastTimestamp'
```

### Port-forward Metrics
```bash
kubectl port-forward -n bolometer-system svc/bolometer-metrics 8080:8080
curl http://localhost:8080/metrics
```

## Documentation

- 📖 **Installation**: `docs/INSTALLATION.md`
- 🚀 **Quick Start**: `docs/QUICKSTART.md`
- 🧪 **Testing**: `docs/TESTING_GUIDE.md`
- 🎨 **Branding**: `BRANDING.md`

## Elevator Pitch

*"Bolometer is a Kubernetes operator that captures Go application profiles when they run hot - when resource usage crosses your defined thresholds. Get actionable performance data when it matters most."*

## Value Propositions

**For DevOps**: Automate profiling without manual intervention
**For Platform Teams**: Deploy once, benefit everywhere
**For Developers**: Stop guessing when to profile

## Social

**Twitter/X**: #BolometerOperator #K8sProfiling #GoLang
**LinkedIn**: #Kubernetes #DevOps #SRE #Observability

## Support

- 📝 GitHub Issues: github.com/bolometer-io/bolometer/issues
- 💬 Discussions: github.com/bolometer-io/bolometer/discussions
- 📧 Email: [TBD]

## License

Apache 2.0

---

**Remember**: Bolometer (capitalized in docs) = bolometer (lowercase in code)

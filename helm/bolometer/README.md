# Bolometer Helm Chart

Bolometer is a Kubernetes operator that automatically captures Go pprof profiles when applications exceed resource thresholds or on-demand.

## Installation

### Add the Helm repository

```bash
helm repo add bolometer https://a-kash-singh.github.io/bolometer
helm repo update
```

### Install the chart

```bash
helm install bolometer bolometer/bolometer \
  --namespace bolometer-system \
  --create-namespace
```

### Install with IRSA (AWS)

```bash
helm install bolometer bolometer/bolometer \
  --namespace bolometer-system \
  --create-namespace \
  --set serviceAccount.annotations."eks\.amazonaws\.com/role-arn"="arn:aws:iam::ACCOUNT_ID:role/bolometer-role"
```

## Configuration

### Key Parameters

| Parameter | Description | Default |
|-----------|-------------|---------|
| `image.repository` | Bolometer image repository | `bolometer` |
| `image.tag` | Bolometer image tag | `latest` |
| `replicaCount` | Number of operator replicas | `1` |
| `serviceAccount.annotations` | Service account annotations (e.g., IRSA) | `{}` |
| `namespace` | Namespace to deploy operator | `bolometer-system` |
| `resources.limits.cpu` | CPU limit | `500m` |
| `resources.limits.memory` | Memory limit | `512Mi` |
| `crd.install` | Install CRDs | `true` |

### Default Profiling Configuration

| Parameter | Description | Default |
|-----------|-------------|---------|
| `defaultConfig.s3.bucket` | S3 bucket for profiles | `""` |
| `defaultConfig.s3.region` | AWS region | `us-west-2` |
| `defaultConfig.s3.prefix` | S3 prefix for profiles | `profiles` |
| `defaultConfig.thresholds.cpuThresholdPercent` | CPU threshold | `80` |
| `defaultConfig.thresholds.memoryThresholdPercent` | Memory threshold | `90` |
| `defaultConfig.thresholds.checkIntervalSeconds` | Check interval | `30` |
| `defaultConfig.thresholds.cooldownSeconds` | Cooldown period | `300` |

## Usage

After installation, create a ProfilingConfig resource:

```yaml
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
    bucket: my-profiling-bucket
    prefix: profiles
    region: us-west-2

  profileTypes:
  - heap
  - cpu
  - goroutine
```

And annotate your pods:

```yaml
apiVersion: v1
kind: Pod
metadata:
  name: my-app
  annotations:
    bolometer.io/enabled: "true"
    bolometer.io/port: "6060"
spec:
  containers:
  - name: app
    image: my-app:latest
```

## Uninstallation

```bash
helm uninstall bolometer -n bolometer-system
kubectl delete namespace bolometer-system
```

## More Information

- [GitHub Repository](https://github.com/a-kash-singh/bolometer)
- [Documentation](https://github.com/a-kash-singh/bolometer/blob/main/README.md)

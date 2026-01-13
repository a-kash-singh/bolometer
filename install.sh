#!/bin/bash

# Bolometer Installation Script
# This script validates prerequisites, prompts for configuration, and installs the operator

set -e

# Colors for output
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
BLUE='\033[0;34m'
NC='\033[0m' # No Color

# Default values
NAMESPACE="bolometer-system"
IMAGE="bolometer:latest"
INSTALL_MODE="kubectl"
SKIP_AWS_VALIDATION="false"
USE_LOCALSTACK="false"

# Script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
CONFIG_DIR="${SCRIPT_DIR}/config"

# Helper functions
log_info() {
    echo -e "${BLUE}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_warning() {
    echo -e "${YELLOW}[WARNING]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
}

print_header() {
    echo ""
    echo "======================================"
    echo "$1"
    echo "======================================"
    echo ""
}

# Validation functions
check_command() {
    if ! command -v "$1" &> /dev/null; then
        log_error "$1 is not installed or not in PATH"
        return 1
    fi
    log_success "$1 is installed"
    return 0
}

check_kubectl_access() {
    if ! kubectl cluster-info &> /dev/null; then
        log_error "Cannot access Kubernetes cluster. Please check your kubeconfig"
        return 1
    fi
    log_success "Kubernetes cluster is accessible"
    return 0
}

check_cluster_version() {
    local version
    version=$(kubectl version --short 2>/dev/null | grep "Server Version" | sed 's/Server Version: v//' | cut -d. -f1,2)

    if [ -z "$version" ]; then
        # Try newer kubectl version command format
        version=$(kubectl version -o json 2>/dev/null | grep -o '"gitVersion":"v[0-9.]*"' | head -1 | sed 's/"gitVersion":"v//' | sed 's/"//' | cut -d. -f1,2)
    fi

    local major minor
    major=$(echo "$version" | cut -d. -f1)
    minor=$(echo "$version" | cut -d. -f2)

    if [ "$major" -lt 1 ] || { [ "$major" -eq 1 ] && [ "$minor" -lt 30 ]; }; then
        log_warning "Kubernetes version $version is below recommended version 1.30+"
        read -p "Continue anyway? (y/n): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            return 1
        fi
    else
        log_success "Kubernetes version $version meets requirements (1.30+)"
    fi
    return 0
}

check_metrics_server() {
    log_info "Checking for metrics-server..."

    if kubectl get deployment metrics-server -n kube-system &> /dev/null; then
        log_success "metrics-server is installed"

        # Check if it's running
        local ready
        ready=$(kubectl get deployment metrics-server -n kube-system -o jsonpath='{.status.readyReplicas}' 2>/dev/null)
        if [ "$ready" -gt 0 ] 2>/dev/null; then
            log_success "metrics-server is running with $ready ready replicas"
        else
            log_warning "metrics-server is installed but not ready"
        fi

        # Try to query metrics API
        if kubectl top nodes &> /dev/null; then
            log_success "metrics-server API is responding"
        else
            log_warning "metrics-server API is not responding yet (may still be starting up)"
        fi
        return 0
    else
        log_error "metrics-server is not installed"
        log_info "The profiling operator requires metrics-server to monitor pod resource usage"
        echo ""
        echo "To install metrics-server, run:"
        echo "  kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml"
        echo ""
        echo "For local/development clusters (kind, minikube), you may need to add --kubelet-insecure-tls:"
        echo "  kubectl patch deployment metrics-server -n kube-system --type='json' -p='[{\"op\": \"add\", \"path\": \"/spec/template/spec/containers/0/args/-\", \"value\": \"--kubelet-insecure-tls\"}]'"
        echo ""
        read -p "Continue installation anyway? (y/n): " -n 1 -r
        echo
        if [[ ! $REPLY =~ ^[Yy]$ ]]; then
            return 1
        fi
        return 0
    fi
}

check_aws_prerequisites() {
    if [ "$SKIP_AWS_VALIDATION" = "true" ]; then
        log_info "Skipping AWS validation"
        return 0
    fi

    log_info "Checking AWS prerequisites for IRSA..."

    # Check if aws CLI is installed (optional but helpful)
    if command -v aws &> /dev/null; then
        log_success "AWS CLI is installed"
    else
        log_warning "AWS CLI is not installed (optional but recommended for AWS deployments)"
    fi

    # Try to detect if this is an EKS cluster
    local cluster_context
    cluster_context=$(kubectl config current-context)

    if [[ $cluster_context == *"eks"* ]] || [[ $cluster_context == *"amazon"* ]]; then
        log_info "Detected EKS cluster: $cluster_context"

        # Check for OIDC provider
        log_info "Note: IRSA requires an OIDC provider to be configured for your EKS cluster"
        log_info "You can check this in the EKS console or with: aws eks describe-cluster --name <cluster-name>"
    else
        log_info "Cluster does not appear to be EKS, IAM roles may not be applicable"
    fi

    return 0
}

# Prompt functions
prompt_for_config() {
    print_header "Configuration"

    # Installation mode
    echo "Installation mode:"
    echo "  1) kubectl (recommended for production)"
    echo "  2) LocalStack (for local testing)"
    read -p "Select mode [1]: " mode_choice
    mode_choice=${mode_choice:-1}

    if [ "$mode_choice" = "2" ]; then
        USE_LOCALSTACK="true"
        SKIP_AWS_VALIDATION="true"
        log_info "Using LocalStack mode"
    fi

    # Namespace
    read -p "Namespace [$NAMESPACE]: " ns_input
    NAMESPACE=${ns_input:-$NAMESPACE}

    # Container image
    read -p "Container image [$IMAGE]: " img_input
    IMAGE=${img_input:-$IMAGE}

    if [ "$IMAGE" = "bolometer:latest" ]; then
        log_warning "Using local image 'bolometer:latest'"
        log_warning "Make sure you have built the image with: make docker-build"
        log_warning "For production, use a registry image like: your-registry/bolometer:v0.1.0"
    fi

    # S3 Configuration
    echo ""
    log_info "S3 Configuration (required for profile storage)"
    read -p "S3 bucket name: " S3_BUCKET

    if [ -z "$S3_BUCKET" ]; then
        log_error "S3 bucket name is required"
        exit 1
    fi

    read -p "AWS region [us-west-2]: " AWS_REGION
    AWS_REGION=${AWS_REGION:-us-west-2}

    if [ "$USE_LOCALSTACK" = "false" ]; then
        # IAM Role for IRSA
        echo ""
        log_info "IAM Role for Service Account (IRSA)"
        log_info "Leave empty to skip IRSA annotation (not recommended for production)"
        read -p "IAM Role ARN (e.g., arn:aws:iam::123456789012:role/bolometer-role): " IAM_ROLE_ARN

        if [ -z "$IAM_ROLE_ARN" ]; then
            log_warning "No IAM role provided. The operator will need AWS credentials from instance profile or environment"
        fi
    else
        # LocalStack configuration
        IAM_ROLE_ARN=""
        log_info "LocalStack endpoint will be configured in the deployment"
    fi

    # Summary
    echo ""
    print_header "Installation Summary"
    echo "Namespace:          $NAMESPACE"
    echo "Image:              $IMAGE"
    echo "S3 Bucket:          $S3_BUCKET"
    echo "AWS Region:         $AWS_REGION"
    if [ -n "$IAM_ROLE_ARN" ]; then
        echo "IAM Role ARN:       $IAM_ROLE_ARN"
    fi
    if [ "$USE_LOCALSTACK" = "true" ]; then
        echo "Mode:               LocalStack"
    fi
    echo ""

    read -p "Proceed with installation? (y/n): " -n 1 -r
    echo
    if [[ ! $REPLY =~ ^[Yy]$ ]]; then
        log_info "Installation cancelled"
        exit 0
    fi
}

# Installation functions
create_namespace() {
    log_info "Creating namespace $NAMESPACE..."

    if kubectl get namespace "$NAMESPACE" &> /dev/null; then
        log_success "Namespace $NAMESPACE already exists"
    else
        kubectl create namespace "$NAMESPACE"
        log_success "Namespace $NAMESPACE created"
    fi
}

install_crds() {
    log_info "Installing Custom Resource Definitions..."

    if [ ! -f "${CONFIG_DIR}/crd/bolometer.io_profilingconfigs.yaml" ]; then
        log_error "CRD file not found: ${CONFIG_DIR}/crd/bolometer.io_profilingconfigs.yaml"
        return 1
    fi

    kubectl apply -f "${CONFIG_DIR}/crd/bolometer.io_profilingconfigs.yaml"
    log_success "CRDs installed"

    # Wait for CRD to be established
    log_info "Waiting for CRD to be established..."
    kubectl wait --for=condition=Established --timeout=60s crd/profilingconfigs.bolometer.io
    log_success "CRD is ready"
}

create_service_account() {
    log_info "Creating service account..."

    # Create a temporary file with the service account
    local sa_file="/tmp/bolometer-sa.yaml"

    cat > "$sa_file" <<EOF
apiVersion: v1
kind: ServiceAccount
metadata:
  name: bolometer
  namespace: $NAMESPACE
EOF

    # Add IRSA annotation if provided
    if [ -n "$IAM_ROLE_ARN" ]; then
        cat >> "$sa_file" <<EOF
  annotations:
    eks.amazonaws.com/role-arn: $IAM_ROLE_ARN
EOF
        log_info "Adding IRSA annotation: $IAM_ROLE_ARN"
    fi

    kubectl apply -f "$sa_file"
    rm "$sa_file"
    log_success "Service account created"
}

install_rbac() {
    log_info "Installing RBAC resources..."

    # Apply role
    if [ ! -f "${CONFIG_DIR}/rbac/role.yaml" ]; then
        log_error "Role file not found: ${CONFIG_DIR}/rbac/role.yaml"
        return 1
    fi
    kubectl apply -f "${CONFIG_DIR}/rbac/role.yaml"

    # Apply role binding
    if [ ! -f "${CONFIG_DIR}/rbac/role_binding.yaml" ]; then
        log_error "Role binding file not found: ${CONFIG_DIR}/rbac/role_binding.yaml"
        return 1
    fi
    kubectl apply -f "${CONFIG_DIR}/rbac/role_binding.yaml"

    log_success "RBAC resources installed"
}

deploy_operator() {
    log_info "Deploying operator..."

    # Create a temporary deployment file with customizations
    local deploy_file="/tmp/bolometer-deployment.yaml"

    # Start with the namespace
    cat > "$deploy_file" <<EOF
apiVersion: v1
kind: Namespace
metadata:
  name: $NAMESPACE
---
apiVersion: apps/v1
kind: Deployment
metadata:
  name: bolometer
  namespace: $NAMESPACE
  labels:
    app: bolometer
    control-plane: controller-manager
spec:
  replicas: 1
  selector:
    matchLabels:
      app: bolometer
      control-plane: controller-manager
  template:
    metadata:
      labels:
        app: bolometer
        control-plane: controller-manager
    spec:
      serviceAccountName: bolometer
      containers:
      - name: manager
        image: $IMAGE
        imagePullPolicy: IfNotPresent
        command:
        - /manager
        args:
        - --leader-elect
EOF

    # Add LocalStack environment variables if needed
    if [ "$USE_LOCALSTACK" = "true" ]; then
        cat >> "$deploy_file" <<EOF
        env:
        - name: AWS_ENDPOINT_URL
          value: "http://localstack.default.svc.cluster.local:4566"
        - name: AWS_ACCESS_KEY_ID
          value: "test"
        - name: AWS_SECRET_ACCESS_KEY
          value: "test"
EOF
    fi

    # Continue with the rest of the deployment spec
    cat >> "$deploy_file" <<EOF
        ports:
        - containerPort: 8080
          name: metrics
          protocol: TCP
        - containerPort: 8081
          name: health
          protocol: TCP
        livenessProbe:
          httpGet:
            path: /healthz
            port: health
          initialDelaySeconds: 15
          periodSeconds: 20
        readinessProbe:
          httpGet:
            path: /readyz
            port: health
          initialDelaySeconds: 5
          periodSeconds: 10
        resources:
          limits:
            cpu: 500m
            memory: 512Mi
          requests:
            cpu: 100m
            memory: 128Mi
        securityContext:
          allowPrivilegeEscalation: false
          capabilities:
            drop:
            - ALL
          runAsNonRoot: true
          runAsUser: 65532
      securityContext:
        runAsNonRoot: true
        seccompProfile:
          type: RuntimeDefault
      terminationGracePeriodSeconds: 10
EOF

    kubectl apply -f "$deploy_file"
    rm "$deploy_file"
    log_success "Operator deployed"
}

wait_for_operator() {
    log_info "Waiting for operator to be ready..."

    if ! kubectl wait --for=condition=available --timeout=120s \
        deployment/bolometer -n "$NAMESPACE" 2>/dev/null; then
        log_warning "Deployment did not become available within 120 seconds"
        log_info "Checking pod status..."
        kubectl get pods -n "$NAMESPACE" -l app=bolometer
        echo ""
        log_info "Checking recent events..."
        kubectl get events -n "$NAMESPACE" --sort-by='.lastTimestamp' | tail -10
        echo ""
        log_warning "Please check the logs with: kubectl logs -n $NAMESPACE -l app=bolometer"
        return 1
    fi

    log_success "Operator is ready"
    return 0
}

validate_installation() {
    print_header "Validating Installation"

    # Check deployment
    log_info "Checking deployment status..."
    local ready_replicas
    ready_replicas=$(kubectl get deployment bolometer -n "$NAMESPACE" -o jsonpath='{.status.readyReplicas}' 2>/dev/null || echo "0")

    if [ "$ready_replicas" -gt 0 ] 2>/dev/null; then
        log_success "Deployment is running with $ready_replicas ready replicas"
    else
        log_error "Deployment is not ready"
        return 1
    fi

    # Check pods
    log_info "Checking pod status..."
    kubectl get pods -n "$NAMESPACE" -l app=bolometer

    # Get pod name
    local pod_name
    pod_name=$(kubectl get pods -n "$NAMESPACE" -l app=bolometer -o jsonpath='{.items[0].metadata.name}' 2>/dev/null)

    if [ -z "$pod_name" ]; then
        log_error "No pods found"
        return 1
    fi

    log_success "Pod $pod_name is running"

    # Check health endpoint
    log_info "Checking health endpoint..."
    if kubectl exec -n "$NAMESPACE" "$pod_name" -- wget -q -O- http://localhost:8081/healthz 2>/dev/null | grep -q "ok"; then
        log_success "Health endpoint is responding"
    else
        log_warning "Health endpoint is not responding (pod may still be starting)"
    fi

    # Check CRD
    log_info "Checking CRD..."
    if kubectl get crd profilingconfigs.bolometer.io &> /dev/null; then
        log_success "ProfilingConfig CRD is installed"
    else
        log_error "ProfilingConfig CRD is not found"
        return 1
    fi

    return 0
}

print_next_steps() {
    print_header "Installation Complete!"

    echo "Next steps:"
    echo ""
    echo "1. Create a ProfilingConfig resource:"
    echo "   kubectl apply -f ${CONFIG_DIR}/samples/profiling_v1alpha1_profilingconfig.yaml"
    echo ""
    echo "2. Annotate your Go application pods:"
    echo "   kubectl annotate pod <pod-name> bolometer.io/enabled=true"
    echo ""
    echo "3. Monitor the operator logs:"
    echo "   kubectl logs -n $NAMESPACE -l app=bolometer -f"
    echo ""
    echo "4. Check ProfilingConfig status:"
    echo "   kubectl get profilingconfigs -A"
    echo ""
    echo "5. View operator metrics:"
    echo "   kubectl port-forward -n $NAMESPACE svc/bolometer-metrics 8080:8080"
    echo "   curl http://localhost:8080/metrics"
    echo ""

    if [ "$USE_LOCALSTACK" = "true" ]; then
        echo "LocalStack Mode:"
        echo "  - Make sure LocalStack is running: kubectl get pods -n default -l app=localstack"
        echo "  - Profiles will be uploaded to LocalStack S3"
        echo "  - Access profiles: aws --endpoint-url=http://localhost:4566 s3 ls s3://$S3_BUCKET/"
        echo ""
    fi

    echo "Documentation:"
    echo "  - README: ${SCRIPT_DIR}/README.md"
    echo "  - Quick Start: ${SCRIPT_DIR}/docs/QUICKSTART.md"
    echo "  - Testing Guide: ${SCRIPT_DIR}/docs/TESTING.md"
    echo ""
}

# Main installation flow
main() {
    print_header "Profiling Operator Installation"

    log_info "Starting installation script..."

    # Validate prerequisites
    print_header "Validating Prerequisites"

    check_command kubectl || exit 1
    check_kubectl_access || exit 1
    check_cluster_version || exit 1
    check_metrics_server || exit 1
    check_aws_prerequisites || exit 1

    # Prompt for configuration
    prompt_for_config

    # Perform installation
    print_header "Installing Operator"

    create_namespace || exit 1
    install_crds || exit 1
    create_service_account || exit 1
    install_rbac || exit 1
    deploy_operator || exit 1

    # Wait and validate
    if wait_for_operator; then
        sleep 5  # Give it a moment to fully stabilize
        validate_installation || {
            log_warning "Installation completed but validation had issues"
            log_info "Please check the operator logs for any errors"
        }
    else
        log_warning "Installation completed but operator is not ready yet"
        log_info "Please monitor the deployment: kubectl get pods -n $NAMESPACE -w"
    fi

    # Print next steps
    print_next_steps
}

# Parse command line arguments
while [[ $# -gt 0 ]]; do
    case $1 in
        --namespace)
            NAMESPACE="$2"
            shift 2
            ;;
        --image)
            IMAGE="$2"
            shift 2
            ;;
        --skip-aws-validation)
            SKIP_AWS_VALIDATION="true"
            shift
            ;;
        --localstack)
            USE_LOCALSTACK="true"
            SKIP_AWS_VALIDATION="true"
            shift
            ;;
        --help)
            echo "Usage: $0 [options]"
            echo ""
            echo "Options:"
            echo "  --namespace <name>       Namespace to install into (default: bolometer-system)"
            echo "  --image <image>          Container image to use (default: bolometer:latest)"
            echo "  --skip-aws-validation    Skip AWS/IRSA validation checks"
            echo "  --localstack            Configure for LocalStack (sets AWS endpoint)"
            echo "  --help                   Show this help message"
            echo ""
            echo "Examples:"
            echo "  $0                                    # Interactive installation"
            echo "  $0 --namespace my-namespace           # Install to custom namespace"
            echo "  $0 --localstack                       # Install for LocalStack testing"
            echo ""
            exit 0
            ;;
        *)
            log_error "Unknown option: $1"
            echo "Use --help for usage information"
            exit 1
            ;;
    esac
done

# Run main installation
main

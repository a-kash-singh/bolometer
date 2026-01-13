#!/bin/bash

# Colors
RED='\033[0;31m'
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
NC='\033[0m'

echo "=========================================="
echo "Cleaning up test environment"
echo "=========================================="
echo ""

# Configuration
OPERATOR_NS="bolometer-system"
TEST_APP_NAMESPACE="demo"

info() {
    echo -e "${YELLOW}→ $1${NC}"
}

success() {
    echo -e "${GREEN}✓ $1${NC}"
}

# Delete ProfilingConfigs
info "Deleting ProfilingConfigs..."
kubectl delete profilingconfig --all -A --ignore-not-found=true
success "ProfilingConfigs deleted"

# Delete sample app
info "Deleting sample application..."
kubectl delete -f examples/target-app.yaml --ignore-not-found=true
success "Sample app deleted"

# Delete LocalStack
info "Deleting LocalStack..."
kubectl delete namespace localstack --ignore-not-found=true
success "LocalStack deleted"

# Delete operator
info "Deleting operator..."
kubectl delete -f config/manager/ --ignore-not-found=true
success "Operator deleted"

# Delete RBAC
info "Deleting RBAC..."
kubectl delete -f config/rbac/ --ignore-not-found=true
success "RBAC deleted"

# Delete CRDs
info "Deleting CRDs..."
kubectl delete -f config/crd/ --ignore-not-found=true
success "CRDs deleted"

# Delete namespaces
info "Deleting test namespaces..."
kubectl delete namespace $TEST_APP_NAMESPACE --ignore-not-found=true
kubectl delete namespace $OPERATOR_NS --ignore-not-found=true
kubectl delete namespace profiling-test --ignore-not-found=true
success "Namespaces deleted"

echo ""
echo -e "${GREEN}=========================================="
echo "Cleanup completed!"
echo "==========================================${NC}"
echo ""
echo "To recreate test environment, run: ./test-e2e.sh"



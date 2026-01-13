#!/bin/bash

set -e

# Colors
GREEN='\033[0;32m'
YELLOW='\033[1;33m'
RED='\033[0;31m'
NC='\033[0m'

log_info() {
    echo -e "${YELLOW}[INFO]${NC} $1"
}

log_success() {
    echo -e "${GREEN}[SUCCESS]${NC} $1"
}

log_error() {
    echo -e "${RED}[ERROR]${NC} $1"
    exit 1
}

# Check if helm is installed
if ! command -v helm &> /dev/null; then
    log_error "helm is not installed. Please install helm first."
fi

# Get script directory
SCRIPT_DIR="$(cd "$(dirname "${BASH_SOURCE[0]}")" && pwd)"
PROJECT_ROOT="$(cd "$SCRIPT_DIR/.." && pwd)"
CHART_DIR="$PROJECT_ROOT/helm/bolometer"
PACKAGE_DIR="$PROJECT_ROOT/.helm-charts"

log_info "Bolometer Helm Chart Release Script"
echo ""

# Validate chart
log_info "Validating Helm chart..."
if ! helm lint "$CHART_DIR"; then
    log_error "Helm chart validation failed"
fi
log_success "Chart validation passed"

# Get chart version
CHART_VERSION=$(grep '^version:' "$CHART_DIR/Chart.yaml" | awk '{print $2}')
log_info "Chart version: $CHART_VERSION"

# Create package directory
mkdir -p "$PACKAGE_DIR"

# Package chart
log_info "Packaging Helm chart..."
helm package "$CHART_DIR" -d "$PACKAGE_DIR"
log_success "Chart packaged: bolometer-${CHART_VERSION}.tgz"

# Generate/update index
log_info "Generating Helm repository index..."
helm repo index "$PACKAGE_DIR" --url https://a-kash-singh.github.io/bolometer
log_success "Repository index generated"

echo ""
log_success "Helm chart release prepared successfully!"
echo ""
log_info "To publish to GitHub Pages:"
echo "  1. Commit the changes in .helm-charts/"
echo "  2. Push to main branch"
echo "  3. The GitHub Action will automatically publish to gh-pages"
echo ""
log_info "Or manually copy .helm-charts/* to gh-pages branch"

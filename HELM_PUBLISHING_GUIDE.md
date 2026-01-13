# Helm Chart Publishing Guide

This guide explains how to publish the Bolometer Helm chart to a public Helm repository using GitHub Pages.

## Overview

We use GitHub Pages to host the Helm repository and GitHub Actions to automate the release process.

## Setup Instructions

### 1. Push Your Repository to GitHub

First, create a new repository on GitHub named `bolometer`, then push your code:

```bash
git remote add origin git@github.com:a-kash-singh/bolometer.git
git push -u origin main
```

### 2. Enable GitHub Pages

1. Go to your repository on GitHub: `https://github.com/a-kash-singh/bolometer`
2. Click on **Settings** → **Pages**
3. Under "Source", select **Deploy from a branch**
4. Select branch: **gh-pages** and folder: **/ (root)**
5. Click **Save**

### 3. Initial Chart Release (First Time Only)

The GitHub Action will automatically create the `gh-pages` branch and publish charts when you push changes to the `helm/` directory. However, for the first release, you may want to trigger it manually:

#### Option A: Wait for Automatic Release
Simply push a change to the `helm/` directory and the action will run automatically.

#### Option B: Manual First Release
Create the gh-pages branch manually:

```bash
# Create an empty gh-pages branch
git checkout --orphan gh-pages
git rm -rf .
echo "# Bolometer Helm Repository" > README.md
git add README.md
git commit -m "Initialize gh-pages branch"
git push origin gh-pages

# Switch back to main
git checkout main
```

Then trigger the workflow by pushing a change to helm/:

```bash
# Make a small change to trigger the workflow
cd helm/bolometer
# Update the chart version if needed
git add Chart.yaml
git commit -m "Trigger helm release"
git push origin main
```

### 4. Verify the Release

After the GitHub Action runs:

1. Check the **Actions** tab in your repository to see the workflow status
2. Once complete, your chart will be available at: `https://a-kash-singh.github.io/bolometer`
3. Check that `index.yaml` exists at that URL

## Using Your Published Helm Chart

Once published, users can install your chart with:

```bash
# Add your Helm repository
helm repo add bolometer https://a-kash-singh.github.io/bolometer

# Update repositories
helm repo update

# Install Bolometer
helm install bolometer bolometer/bolometer \
  --namespace bolometer-system \
  --create-namespace
```

## Releasing New Versions

### Automated Release (Recommended)

1. Update the chart version in `helm/bolometer/Chart.yaml`:
   ```yaml
   version: 0.2.0  # Increment version
   ```

2. Make your chart changes

3. Commit and push:
   ```bash
   git add helm/bolometer/
   git commit -m "Release Helm chart v0.2.0"
   git push origin main
   ```

4. The GitHub Action will automatically:
   - Package the chart
   - Create a GitHub release
   - Update the Helm repository index
   - Publish to GitHub Pages

### Manual Release

If you prefer to release manually:

```bash
# Run the release script
./scripts/release-helm-chart.sh

# This will create packaged chart in .helm-charts/
# Then manually copy to gh-pages branch or use the GitHub Action
```

## Troubleshooting

### GitHub Action Fails

1. Check the **Actions** tab for error messages
2. Ensure GitHub Pages is enabled
3. Verify the workflow has write permissions:
   - Go to **Settings** → **Actions** → **General**
   - Under "Workflow permissions", select **Read and write permissions**

### Chart Not Showing Up

1. Wait a few minutes for GitHub Pages to update
2. Clear your browser cache
3. Check the gh-pages branch to see if files were committed
4. Verify the index.yaml file is present at `https://a-kash-singh.github.io/bolometer/index.yaml`

### Helm Repo Add Fails

```bash
# Try clearing the cache
helm repo remove bolometer
helm repo add bolometer https://a-kash-singh.github.io/bolometer
helm repo update
```

## Chart Versioning

Follow semantic versioning (SemVer):
- **Major** (1.0.0): Breaking changes
- **Minor** (0.1.0): New features, backward compatible
- **Patch** (0.0.1): Bug fixes

Update both:
- `version`: The chart version
- `appVersion`: The Bolometer application version

```yaml
version: 0.2.0      # Chart version
appVersion: "0.2.0" # Application version
```

## Testing Before Release

Always test your chart before releasing:

```bash
# Lint the chart
helm lint helm/bolometer

# Template the chart (dry-run)
helm template bolometer helm/bolometer

# Install locally from source
helm install bolometer-test helm/bolometer --dry-run --debug
```

## Repository Structure

After publishing, your Helm repository will have this structure:

```
gh-pages branch:
├── index.yaml                    # Repository index
├── bolometer-0.1.0.tgz          # Chart package
├── bolometer-0.2.0.tgz          # Newer version
└── ...
```

## Additional Resources

- [Helm Chart Repository Guide](https://helm.sh/docs/topics/chart_repository/)
- [GitHub Pages Documentation](https://docs.github.com/en/pages)
- [Chart Releaser Action](https://github.com/helm/chart-releaser-action)

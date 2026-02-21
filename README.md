# rollback-controller

A Kubernetes controller that automatically creates GitLab revert commits when a [Flux](https://fluxcd.io/) `Kustomization` or `HelmRelease` enters a failed state.

## How It Works

The controller watches all `Kustomization` and `HelmRelease` resources cluster-wide. When one transitions to `Ready=False`, it records the failing commit SHA and starts a debounce timer. If the resource remains failed for the full debounce window (default: 300 seconds), the controller calls the GitLab commits revert API to create a revert commit on a new branch.

```
Flux resource → Ready=False → debounce timer starts
                            → still failing after N seconds → POST GitLab revert API
                            → recovers before N seconds   → timer cancelled
```

The `revertedSHAs` map (in-memory, not persisted) ensures each failing SHA triggers at most one revert.

## Requirements

- A Kubernetes cluster with [Flux](https://fluxcd.io/) installed
- A GitLab instance with API access
- Go 1.25+ (to build)

## Build

```bash
go build -o rollback-controller .
```

## Configuration

All configuration is via environment variables:

| Variable               | Default            | Description                                      |
|------------------------|--------------------|--------------------------------------------------|
| `GITLAB_TOKEN`         | *(required)*       | GitLab private API token                         |
| `GITLAB_PROJECT_ID`    | *(required)*       | GitLab project ID for revert commits             |
| `GITLAB_URL`           | `https://gitlab`   | GitLab base URL                                  |
| `REVERT_BRANCH_PREFIX` | `revert`           | Prefix for the revert branch name                |
| `DEBOUNCE_SECONDS`     | `300`              | Seconds to wait before triggering a revert       |
| `REVERT_MODE`          |                    | Set to `echo` for dry-run (no GitLab API calls)  |

## Running Locally

```bash
GITLAB_TOKEN=<token> GITLAB_PROJECT_ID=<id> ./rollback-controller
```

Dry-run mode (prints what would be POSTed instead of calling GitLab):

```bash
REVERT_MODE=echo DEBOUNCE_SECONDS=15 GITLAB_PROJECT_ID=42 \
  GITLAB_URL=https://gitlab.example.com ./rollback-controller
```

## Deployment

Apply the CRD and manifests to your cluster:

```bash
kubectl apply -f crds/rollbackpolicy.yaml
kubectl apply -f manifests/deployment.yaml
```

Before applying, update the image reference in `manifests/deployment.yaml`:

```yaml
image: yourrepo/flux-rollback-agent:latest  # replace with your registry path
```

The controller runs in the `flux-system` namespace as the `flux-rollback-agent` service account and requires:

- A `gitlab-token` Secret with a `token` key containing your GitLab API token
- A `kubeconfig` ConfigMap with cluster access (when running outside the cluster)

RBAC permissions (defined in `manifests/deployment.yaml`) grant read access to `kustomizations`, `helmreleases`, and `gitrepositories` in `flux-system`.

## End-to-End Test

The `test/broken-kustomization.yaml` fixture creates a `GitRepository` and a `Kustomization` pointing to an invalid path, which causes Flux to set `Ready=False`.

```bash
kubectl apply -f test/broken-kustomization.yaml

# Wait for GitRepository to become ready (~30s), then inject the revision:
kubectl patch kustomization test-broken -n flux-system \
  --subresource=status --type=merge \
  -p '{"status":{"lastAttemptedRevision":"main@sha1:<sha>","observedGeneration":1}}'

# Run with short debounce and echo mode:
REVERT_MODE=echo DEBOUNCE_SECONDS=15 GITLAB_PROJECT_ID=42 \
  GITLAB_URL=https://gitlab.example.com ./rollback-controller

# Clean up:
kubectl delete -f test/broken-kustomization.yaml
```

Expected output after the debounce window:

```
[Kustomization/flux-system/test-broken] Failure detected for SHA ..., will revert after 15s debounce
[Kustomization/flux-system/test-broken] Failure stable for 15s. Creating revert for SHA ...
[ECHO] Would POST .../commits/<sha>/revert -> branch: revert-<sha>
```

## Architecture

The entire controller is a single file (`main.go`, ~165 lines).

**Core types:**

- `RollbackController` — holds GitLab credentials, debounce config, and two in-memory maps: `pendingSHAs` (first-seen timestamps) and `completedSHAs` (already-reverted SHAs).
- `GenericReconciler` — wraps `RollbackController` and implements `ctrl.Reconciler`. A single instance handles both `Kustomization` and `HelmRelease` resources.

**Reconciliation flow:**

1. `GenericReconciler.Reconcile()` tries to fetch the object as a `Kustomization`; if that fails, it tries `HelmRelease`.
2. It checks for a `Ready=False` condition and extracts `LastAttemptedRevision` as the SHA.
3. `handleResource()` implements the debounce logic and calls `createGitlabRevertMR()` when the window expires.
4. `createGitlabRevertMR()` calls `POST /projects/:id/repository/commits/:sha/revert` with a branch named `<prefix>-<sha>`.

**Note:** The `RollbackPolicy` CRD is defined but not yet reconciled — the controller currently watches all Kustomizations and HelmReleases cluster-wide.

# CLAUDE.md

This file provides guidance to Claude Code (claude.ai/code) when working with code in this repository.

## Build and Run

```bash
# Build the binary
go build -o rollback-controller .

# Format code and tidy deps
gofmt -w . && go mod tidy

# Run locally (requires kubeconfig and env vars)
GITLAB_TOKEN=<token> GITLAB_PROJECT_ID=<id> ./rollback-controller
```

Required environment variables at runtime:
- `GITLAB_TOKEN` — GitLab private API token
- `GITLAB_PROJECT_ID` — GitLab project ID for revert commits
- `GITLAB_URL` — GitLab base URL (default: `https://gitlab`)
- `REVERT_BRANCH_PREFIX` — Branch name prefix (default: `revert`)
- `DEBOUNCE_SECONDS` — Debounce window before triggering revert (default: `300`)
- `REVERT_MODE=echo` — Dry-run mode: prints what would be POSTed instead of calling GitLab

## End-to-End Test

The `test/broken-kustomization.yaml` fixture creates a GitRepository (valid public repo) and a Kustomization pointing to an invalid path — Flux marks it `Ready=False`.

Because the path-not-found failure happens before Flux sets `lastAttemptedRevision`, patch it manually:

```bash
kubectl apply -f test/broken-kustomization.yaml

# Wait for GitRepository to become ready (~30s), then inject the revision:
kubectl patch kustomization test-broken -n flux-system \
  --subresource=status --type=merge \
  -p '{"status":{"lastAttemptedRevision":"main@sha1:<sha>","observedGeneration":1}}'

# Run controller with short debounce and echo mode:
REVERT_MODE=echo DEBOUNCE_SECONDS=15 GITLAB_PROJECT_ID=42 \
  GITLAB_URL=https://gitlab.example.com ./rollback-controller

# Expected output (after ~15s):
# [Kustomization/flux-system/test-broken] Failure detected for SHA ..., will revert after 15s debounce
# [Kustomization/flux-system/test-broken] Failure stable for 15s. Creating revert for SHA ...
# [ECHO] Would POST .../commits/<sha>/revert -> branch: revert-<sha>

# Clean up:
kubectl delete -f test/broken-kustomization.yaml
```

In production deployments where a commit breaks a kustomize apply (valid path, invalid manifests), Flux sets `lastAttemptedRevision` automatically — no manual patch needed.

## Architecture

The entire controller lives in a single file (`main.go`, ~165 lines). There are no packages, no sub-directories with Go code.

**Core types:**
- `RollbackController` — holds GitLab credentials, debounce config, and the `revertedSHAs` in-memory map that tracks which commit SHAs have already triggered a revert. This map is the only state; it is not persisted.
- `GenericReconciler` — wraps `RollbackController` and implements `ctrl.Reconciler`. A single reconciler instance handles both `Kustomization` and `HelmRelease` resources by attempting a `Get` for each type.

**Reconciliation flow:**
1. `main()` registers a single `GenericReconciler` that watches both `kustomizev1.Kustomization` (primary) and `helmv1.HelmRelease` (via `Watches`).
2. On each reconcile, `GenericReconciler.Reconcile()` tries to fetch the object as a Kustomization; if that fails, it tries HelmRelease.
3. It checks for a `Ready=False` condition and extracts `LastAppliedRevision` as the SHA.
4. `handleResource()` implements the debounce: first failure records a timestamp and returns; subsequent calls after `DebounceSeconds` (hardcoded 300s) trigger `createGitlabRevertMR()`.
5. `createGitlabRevertMR()` calls the GitLab commits revert API (`POST /projects/:id/repository/commits/:sha/revert`) with a branch named `<prefix>-<sha>`.

**Note:** The GitLab API base URL is hardcoded as `https://gitlab/...` — this assumes an internal DNS name `gitlab`. Update this if targeting a different host.

## Deployment

Apply the CRD and manifests:

```bash
kubectl apply -f crds/rollbackpolicy.yaml
kubectl apply -f manifests/deployment.yaml
```

The controller runs in the `flux-system` namespace as the `flux-rollback-agent` service account. It needs a `gitlab-token` Secret and a `kubeconfig` ConfigMap in that namespace. The image reference in `manifests/deployment.yaml` (`yourrepo/flux-rollback-agent:latest`) must be updated to a real registry path before use.

The `RollbackPolicy` CRD (`toolkit.fluxcd.io/v1alpha1`) is defined but not yet reconciled by the controller — the controller currently watches all Kustomizations and HelmReleases cluster-wide rather than reading `RollbackPolicy` resources.

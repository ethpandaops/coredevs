# Deploying coredevs to the platform analytics cluster

These manifests target the **analytics** cluster in `ethpandaops/platform` and
follow its conventions (custom Helm chart + an `mca-deployer2` ApplicationSet,
managed by the primary cluster's ArgoCD, deployed via the ArgoCD Vault Plugin).

`platform` is deploy-only — it builds no images. The container image is built by
this repo's CI (`.github/workflows/docker.yaml`) and published to
`ghcr.io/ethpandaops/coredevs`. **Land these manifests only after the image is
published**, otherwise ArgoCD will sync a StatefulSet that can't pull.

## File placement

Copy into the platform staging tree (staging is the source of truth; production
is a promoted copy — never edit it directly):

| This repo | platform repo |
| --- | --- |
| `applications/coredevs/` | `environments/staging/applications/coredevs/` |
| `argocd/coredevs.yaml` | `environments/staging/clusters/primary/kubernetes/argocd/mca-deployer2/deploy/templates/coredevs.yaml` |

## Steps

1. **Publish the image** — push this repo to `github.com/ethpandaops/coredevs`;
   CI builds `ghcr.io/ethpandaops/coredevs:latest` (+ `:<sha>`).
2. **Add the app to staging** — copy the files above into the platform staging
   tree. Commit straight to `master` (platform staging deploys from master HEAD
   via ArgoCD; no PR).
3. **Smoke-test in staging** — `values/staging.yaml` ships `replicaCount: 0`.
   Flip it to `1` (and `ingress.enabled: true` if you want a staging URL) to
   bring it up, then check:
   - `kubectl --context <analytics-staging> -n coredevs get pods`
   - `curl https://coredevs.analytics.staging.platform.ethpandaops.io/api/v1/teams`
4. **Promote to production**:
   ```bash
   ./promote.sh application coredevs staging production
   ./promote.sh cluster-app primary mca-deployer2 staging production
   ```
5. Production serves at
   `https://coredevs.analytics.production.platform.ethpandaops.io`.

## Optional GitHub token

The token only lifts the rate limit on the on-demand `/api/v1/orgs/{org}/members`
endpoint (the synced github-org source makes no calls until a team opts an org
in). To enable it:

1. Add `github_token` under a `coredevs` key in
   `environments/staging/clusters/primary/secrets/...` (backport into staging,
   then promote — never hand-edit production secrets).
2. Set `github.tokenEnabled: true` in `values.yaml`.

## Notes

- **StatefulSet, 1 replica, 1Gi `local-path` PVC** at `/data` holding the
  last-good index snapshot — there is no database.
- The team registry lives in `values.yaml` (`teams:`) and is rendered into the
  app's ConfigMap, so teams/org-wiring can change without rebuilding the image.
- `/metrics` is scraped via the ServiceMonitor on the single HTTP port (8080).

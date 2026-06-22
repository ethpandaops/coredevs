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

## Postgres password secret

The writer and readers share a Postgres holding one canonical snapshot row, so
every pod serves identical data. Add a `db_password` under the `coredevs` key in
the staging secrets (backport into staging, then promote — never hand-edit
production secrets), alongside the optional `github_token`. The bundled Postgres
(`postgres.provision: true`) and the app DSN both read it. To use an external or
managed Postgres instead, set `postgres.provision: false` and point the DSN at
it.

## Architecture

coredevs holds no authoritative state — the index is re-derived from upstream and
the keys are a cache of `github.com/<handle>.keys` — so it runs shared-nothing
behind a shared Postgres:

- **`coredevs-writer`** (Deployment, 1 replica, `--role=writer`, Recreate): the
  single pod that syncs upstream, fetches keys and publishes one snapshot row to
  Postgres. Receives no user traffic. Its brief downtime during a deploy does not
  affect serving.
- **`coredevs-reader`** (Deployment, ≥3 replicas, `--role=reader`,
  RollingUpdate `maxUnavailable=0`, HPA, PDB): stateless serving pods. Each polls
  the snapshot into memory and serves it, so all readers return identical data —
  no split brain. They never call GitHub, and serve the last snapshot if Postgres
  blips, so a Postgres restart never drops traffic. The public `Service` and
  `Ingress` target readers only.
- **`coredevs-postgres`** (bundled StatefulSet, single instance): the shared
  source of truth. Readers cache it in memory, so it is not in the per-request
  path.

Notes:

- **The team registry is the repo's `config.yaml`, baked into the image** (run
  with `--config=/app/config.yaml`) — the single source of truth. To change
  teams/rosters/org-wiring: edit `config.yaml` at the repo root, push, and the
  new image auto-deploys. The per-pod `cluster.role` is set by the `--role` flag
  on each Deployment (the config is one baked file).
- **Auto-pull via Keel** (`pullPolicy: Always`): Keel watches
  `ghcr.io/ethpandaops/coredevs:latest` and force-updates both Deployments when a
  new image lands; the reader RollingUpdate keeps serving zero-downtime.
- `/metrics` is scraped via the ServiceMonitor on the HTTP port (8080).

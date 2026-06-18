# coredevs

An index of Ethereum core developers, assembled from multiple datasources and
exposed over a small HTTP API.

`coredevs` answers questions like *"which GitHub handles are on the Teku team?"*
by taking the **superset** of contributors across datasources, per team. It
exists to replace hand-maintained lists of client-team GitHub handles (for
example the per-client `authorized_keys` lists in devnet tooling) with an
endpoint that refreshes itself.

## Datasources

| Source | What it provides |
| --- | --- |
| `protocol-guild` | Parses the [Protocol Guild membership document](https://github.com/protocolguild/documentation/blob/main/docs/01-membership.md) and maps each working-group section (Teku, Lighthouse, Geth, …) onto a canonical team. This is the precise per-client signal. |
| `github-org` | Resolves the **public** members of a GitHub organisation (`GET /orgs/{org}/public_members`). Available on demand for any org; only folded into a team's superset when that team explicitly opts an org in (see below). |

Sources refresh every `syncInterval` (default 3h). The last good result of each
source is kept in memory, so a transient upstream outage never drops a team. The
built index is persisted to `snapshotPath` and reloaded on restart, so the
service serves last-known-good data immediately on boot.

## Superset semantics & the org caveat

A team's member list is the union of the handles its configured sources report,
deduplicated case-insensitively, with provenance (which sources/orgs each handle
came from) retained.

By default teams are sourced from **Protocol Guild only**. The obvious candidate
GitHub orgs (`Consensys`, `OffchainLabs`, `ChainSafe`, `status-im`,
`NethermindEth`, …) are whole-company orgs whose public membership is much
broader than the client dev team — wiring them into a team's superset pollutes
it with unrelated employees. Opt an org into a team (via `githubOrgs` in the
config) only once you've confirmed its membership approximates the team. Any org
can always be queried ad-hoc via `/api/v1/orgs/{org}/members` without wiring.

## API

| Method & path | Description |
| --- | --- |
| `GET /api/v1/teams` | All teams with per-source member counts. |
| `GET /api/v1/users/{team}` | The superset of handles for a team. |
| `GET /api/v1/handles/{handle}` | Reverse lookup: every team a handle appears on. |
| `GET /api/v1/orgs/{org}/members` | Public members of an arbitrary GitHub org, on demand. |
| `GET /api/v1/sources` | Per-source sync status (last attempt/success/error). |
| `GET /api/v1/export` | The full index as JSON. |
| `GET /healthz` | Liveness. |
| `GET /readyz` | Readiness — 503 until the first index is available. |
| `GET /metrics` | Prometheus metrics. |

### `GET /api/v1/users/{team}` parameters

- `source=protocol-guild|github-org` — restrict to a single source.
- `format=json|txt|yaml` — `json` (default) returns handles plus provenance;
  `txt` returns newline-separated handles; `yaml` returns a YAML list of handles
  (drops straight into an ansible variable).

```bash
# Ansible-ready handle list for the Teku team
curl https://coredevs.example/api/v1/users/teku?format=yaml

# Only the Protocol Guild members of Lighthouse
curl https://coredevs.example/api/v1/users/lighthouse?source=protocol-guild

# Who publicly lists the sigp org on their profile
curl https://coredevs.example/api/v1/orgs/sigp/members
```

## Configuration

See [`config.yaml`](./config.yaml). A `GITHUB_TOKEN` environment variable is
optional but lifts the unauthenticated GitHub rate limit.

## Running

```bash
go run ./cmd/coredevs --config config.yaml
# or
make run
```

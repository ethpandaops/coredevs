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
| `manual` | Static handles listed per team in config (`members:`). For people who belong to a team but aren't yet in an upstream source — e.g. a new joiner before they're added to Protocol Guild. Always included; not subject to rate limits or the member floor. |

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

Teams span four `kind`s — `client` (the 11 EL/CL clients), `research`,
`coordination` (EL/CL spec coordination), and `delivery` (testing, devnets,
security, and the ethPandaOps team) — mirroring the Protocol Guild working
groups plus a few non-PG teams. Filter with `/api/v1/teams?kind=client` to get
just the client teams.

| Method & path | Description |
| --- | --- |
| `GET /api/v1/teams` | All teams with per-source member counts. `?kind=` filters by role. |
| `GET /api/v1/users/{team}` | The superset of handles for a team. |
| `GET /api/v1/users/{team}/keys` | The assembled `authorized_keys` for a team (see below). |
| `GET /api/v1/handles/{handle}` | Reverse lookup: every team a handle appears on. |
| `GET /api/v1/handles/{handle}/keys` | A single developer's cached SSH public keys. |
| `GET /api/v1/orgs/{org}/members` | Public members of an arbitrary GitHub org, on demand. |
| `GET /api/v1/sources` | Per-source sync status (last attempt/success/error) plus key-cache status. |
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

## GitHub key proxy

`coredevs` caches each indexed developer's **SSH public keys**
(`https://github.com/<handle>.keys`) so devnet tooling can fetch a whole team's
`authorized_keys` from one place instead of hitting GitHub once per developer.

A single background walker refreshes the keys round-robin, pacing itself so the
work is spread evenly rather than bursting. The delay between fetches is derived
from two knobs in `keys:` config:

```
delay = max(refreshInterval / handleCount, 1 / maxRequestsPerSecond)
```

- `refreshInterval` (default `3h`) is the target staleness — a full pass
  completes about once per window, so a key change shows up within it. Lower it
  for fresher keys.
- `maxRequestsPerSecond` (default `5`) is a hard ceiling on the request rate, so
  a small handle set never bursts against GitHub.

With ~300 handles and a 3h window that is one request every ~36s (~0.03 req/s).
The cache is served from memory, persisted to `keys.snapshotPath`, and reloaded
on restart so keys are available immediately on boot. A transient GitHub failure
keeps the last good keys rather than dropping a developer.

| Path | `format` | Returns |
| --- | --- | --- |
| `GET /api/v1/users/{team}/keys` | `txt` (default) | A ready-to-use `authorized_keys` file: each developer's keys, prefixed by a `# handle` comment. |
| `GET /api/v1/users/{team}/keys` | `json` | Per-handle keys with `fetchedAt` and a `pending` flag for handles not yet fetched. |
| `GET /api/v1/handles/{handle}/keys` | `json` (default) | One developer's keys plus `fetchedAt`. A cold miss for an indexed handle triggers one on-demand fetch. |
| `GET /api/v1/handles/{handle}/keys` | `txt` | Newline-separated keys for one developer. |

```bash
# authorized_keys for the entire Geth team
curl https://coredevs.example/api/v1/users/geth/keys > authorized_keys

# One developer's keys
curl https://coredevs.example/api/v1/handles/karalabe/keys?format=txt
```

The team endpoint serves only what is already cached (the walker keeps it warm);
the single-handle endpoint fetches on demand for a cold, *indexed* handle so a
first request is never empty. Unknown handles return `404` — the endpoint is not
an open proxy for arbitrary GitHub logins.

## Configuration

See [`config.yaml`](./config.yaml). A `GITHUB_TOKEN` environment variable is
optional but lifts the unauthenticated GitHub rate limit.

## Running

```bash
go run ./cmd/coredevs --config config.yaml
# or
make run
```

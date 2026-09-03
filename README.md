# borgbase-operator

A Kubernetes operator that creates and adopts [BorgBase](https://www.borgbase.com)
restic repositories, and renders the CronJobs that back up into them.

It replaces per-app, hand-copied backup manifests with two short custom
resources. Only restic-format repositories are in scope; BorgBase also serves
borg repositories, which this operator deliberately does not manage.

## What it does

- **Creates or adopts** a BorgBase repository, recording its opaque ID in
  `status.repositoryID` so the app-to-repository mapping is visible without
  decrypting anything.
- **Generates the restic password exactly once**, writes it alongside a
  synthesized `RESTIC_REPOSITORY` into a Secret, and never rotates it.
- **Initializes the repository** with a one-shot Job that probes before
  initializing, so a genuine failure is not mistaken for success.
- **Renders a CronJob** from a declarative backup spec: sources, retention,
  database credential wiring, pod affinity, cache volume and healthchecks
  reporting are all derived.

## Resources

### Repository

```yaml
apiVersion: borgbase.clevyr.com/v1
kind: Repository
metadata:
  name: restic
  namespace: myapp-prod
spec:
  # Adopt an existing repository. The operator only ever looks it up: a wrong
  # ID fails loudly rather than provisioning an empty repo beside real backups.
  existingRepositoryID: a1b2c3d4
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD

  # Or omit both of the above to create a new one:
  # region: us
  # quotaGiB: 100
```

`deletionPolicy` defaults to `Retain`: deleting the resource leaves both the
BorgBase repository and the credentials Secret in place. Set it to `Delete`
only when destroying every snapshot is genuinely what you want.

### ScheduledBackup

```yaml
apiVersion: borgbase.clevyr.com/v1
kind: ScheduledBackup
metadata:
  name: restic
  namespace: myapp-prod
spec:
  repositoryRef: {name: restic}
  schedule: "@hourly"          # jittered; a cron expression is used verbatim
  sources:
    - {type: cnpg, tag: db}
    - {type: files, tag: files, path: app, exclude: ["**/temp*"]}
  retention: {hourly: 168, daily: 90, monthly: 24, yearly: 10}
  database: {engine: cnpg, secretName: postgresql-app}
  volume: {existingClaim: myapp-prod-storage}
  healthchecks:
    pingKeySecretRef: {name: healthchecks-ping-key, key: PING_KEY}
```

`spec.script` replaces the generated body wholesale for anything the source
types do not cover; it still gets the standard preamble, logging and cache
cleanup.

#### Schedules

A five-field cron expression is passed through untouched. A shorthand
(`@hourly`, `@daily`, `@weekly`, `@monthly`, `@every 6h`) is expanded with a
jitter derived from the resource's namespace and name, so copy-pasted backups
spread across the period instead of all firing on the hour. The result appears
in `status.effectiveSchedule`. The jitter is stable: a backup keeps its slot
unless it is renamed.

Steps that do not divide their period evenly (`@every 7h`) are rejected, since
cron restarts the sequence each period and would leave an irregular gap.

#### Healthchecks

The operator contains no healthchecks API client. `runitor`, already in the
backup image, pings `<apiURL>/<pingKey>/<slug>?create=1`, and healthchecks
auto-provisions the check on first ping with the project's notification
channels attached.

The ping key is **per resource, never cluster-wide**: separate client projects
have separate ping keys, and a shared one would file checks into the wrong
project. The slug defaults to the namespace.

Auto-created checks get the healthchecks defaults of a one-day period and
one-hour grace. Adopting an existing check preserves its tuned values: set its
`slug` to match, and the first ping returns `200` rather than `201`.

## Configuration

| Flag | Default | Purpose |
| --- | --- | --- |
| `--api-token-secret` | `borgbase-system/borgbase-api` | Default BorgBase API token |
| `--api-token-key` | `token` | Key within that Secret |
| `--backup-image` | `ghcr.io/clevyr/restic:0.18.1` | Must provide restic, runitor, ts, dumpdb |
| `--cache-storage-class` | *(none)* | StorageClass for restic cache volumes |
| `--healthchecks-enabled` | `true` | Report runs via runitor |
| `--healthchecks-api-url` | `http://healthchecks.healthchecks:8000/ping` | Ping endpoint |
| `--healthchecks-auto-create` | `true` | Auto-provision checks on first ping |
| `--borgbase-endpoint` | *(public API)* | Override, for testing |

The BorgBase token needs **Full Access**: `repoAdd` alone is insufficient
because the operator also uses `repoEdit`, and `repoDelete` under the `Delete`
policy.

## Migrating an existing backup

`hack/migrate.sh` reads an app's hand-written `resources/restic` directory,
decrypts its secret to recover the BorgBase repository ID, and emits the two
resources with the current schedule, retention and sources pinned verbatim.

```sh
hack/migrate.sh ../fleet-infra/apps/fennec/myapp/prod/resources/restic > generated.yaml
go run ./hack/parity generated.yaml ../fleet-infra/apps/fennec/myapp/prod/resources/restic/helmrelease.yaml
```

`hack/parity` renders the generated resource and compares the resulting script
against the original, ignoring only the optional quoting around `--exclude`
patterns. Run it for every app before cutting it over: migrating must not
silently change what gets backed up.

Keep the existing SOPS secret, reduced to `RESTIC_PASSWORD`. The operator reads
it as a seed and never writes to it, so it stays the off-cluster copy of the key
that decrypts the backups. `RESTIC_REPOSITORY` is synthesized by the operator
and `CHECK_UUID` is replaced by slug pinging, so both can be dropped.

## Development

```sh
make manifests generate   # regenerate CRDs, RBAC and deepcopy
make test                 # unit tests
make build-installer IMG=...   # render dist/install.yaml
```

Releases publish a container image to `ghcr.io/clevyr/borgbase-operator` and
the rendered manifests as an OCI artifact at
`ghcr.io/clevyr/borgbase-operator-manifests`, which Flux consumes via an
`OCIRepository`.

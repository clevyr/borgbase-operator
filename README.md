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

Two backups in one namespace must set distinct `healthchecks.slug` values. The
slug defaults to the namespace, and sharing one check would let either
backup's success hide the other's failure, so the newer of the two is held
back with `SlugConflict` rather than silently reporting into the same place.

### Pod security

Backup pods drop every capability, forbid privilege escalation, run under the
`RuntimeDefault` seccomp profile and mount no ServiceAccount token.

No user or group id is imposed. A backup has to read the app's own data, whose
ownership varies per app and per cluster, and an `fsGroup` would chown that
data as a side effect of backing it up. A namespace enforcing the restricted
Pod Security Standard supplies what it needs through `spec.podSecurityContext`,
with `spec.containerSecurityContext` for `readOnlyRootFilesystem`. Both replace
the default wholesale.

### Database credentials

`spec.database.secretName` chooses the Secret; it does not choose where the
Secret is mounted. `dumpdb` is invoked without `--secret-mount`, so it reads a
fixed path per engine, and the operator mounts there: `/postgresql-app` for
cnpg and `/mariadb` for mariadb, whatever the Secret is called.

#### Schedules

A five-field cron expression is passed through untouched. A shorthand is
expanded with a jitter derived from the resource's namespace and name, so
copy-pasted backups spread across the period instead of all firing on the hour.
The result appears in `status.effectiveSchedule`. The jitter is stable: a
backup keeps its slot unless it is renamed.

Accepted shorthands are `@hourly`, `@daily` (`@midnight`), `@weekly`,
`@monthly`, `@yearly` (`@annually`) and `@every <duration>`. Stepped schedules
are phase-shifted rather than started at zero, so `@every 15m` renders as
`7-59/15 * * * *` rather than `*/15 * * * *`.

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
| `--api-token-secret` | `borgbase-api` | Default BorgBase API token. A bare name resolves in the operator's own namespace, taken from `POD_NAMESPACE` |
| `--api-token-key` | `token` | Key within that Secret |
| `--backup-image` | `ghcr.io/clevyr/restic:0.18.1` | Must provide restic, runitor, ts, dumpdb |
| `--cache-storage-class` | *(none)* | StorageClass for restic cache volumes |
| `--healthchecks-enabled` | `true` | Report runs via runitor |
| `--healthchecks-api-url` | `http://healthchecks.healthchecks:8000/ping` | Ping endpoint |
| `--healthchecks-auto-create` | `true` | Auto-provision checks on first ping |
| `--borgbase-endpoint` | *(public API)* | Override, for testing |

The BorgBase token needs **Full Access**. `repoAdd` alone is insufficient:
`quotaGiB`, `alertDays` and `appendOnly` are reconciled on every pass with
`repoEdit`, and `repoDelete` is used under the `Delete` policy. Only fields
that actually differ are sent, so a setting changed in the BorgBase UI that
the spec says nothing about is left alone.

## Migrating an existing backup

`hack/migrate.sh` reads an app's hand-written `resources/restic` directory,
decrypts its secret to recover the BorgBase repository ID, and emits the two
resources with the current retention and sources pinned verbatim, and the
schedule handed back to the operator (see below).

```sh
hack/migrate.sh ../fleet-infra/apps/fennec/myapp/prod/resources/restic > generated.yaml
corg parity generated.yaml ../fleet-infra/apps/fennec/myapp/prod/resources/restic/helmrelease.yaml
```

```
RESCHEDULED: "20 0 * * *" -> "33 20 * * *" (every 24h0m0s either way; the operator jittered it)
EQUIVALENT
```

`corg parity` renders the generated resource and compares it against the
original. The script must match, ignoring only the optional quoting around
`--exclude` patterns and the `--retry-lock` flag. The schedule is compared by
**cadence** rather than by the minute it lands on, because migration
deliberately moves the time; see below. It prints `IDENTICAL` when nothing
moved, `EQUIVALENT` when only the schedule was re-jittered, and fails on
anything else. Run it for every app before cutting it over: migrating must not
silently change what gets backed up, or how often.

### Schedules are handed back to the operator

The hand-written schedules are hand-jittered: someone picked a minute to keep
apps off the top of the hour, and copy-paste meant several apps ended up
sharing one anyway. `migrate.sh` converts them to the equivalent shorthand, so
the operator derives the offset from a hash of the resource and the spread is
maintained without anyone maintaining it.

| Hand-written | Migrated to |
| --- | --- |
| `36 * * * *` | `@hourly` |
| `20 0 * * *` | `@daily` |
| `27 */6 * * *` | `@every 6h` |

Anything that does not map exactly, such as a specific weekday, is pinned
verbatim and left alone.

This moves when a backup runs, which is the point, but note two things on
cutover day. A backup whose new slot has already passed will wait until the
next one, so a daily backup can go up to a day longer than usual before its
first run under the operator. If its healthchecks check was auto-created with
the default one-day period and one-hour grace, that can trip a late alarm once.
Neither loses data; both are worth expecting rather than being surprised by.

Keep the existing SOPS secret, reduced to `RESTIC_PASSWORD`. The operator reads
it as a seed and never writes to it, so it stays the off-cluster copy of the key
that decrypts the backups. `RESTIC_REPOSITORY` is synthesized by the operator
and `CHECK_UUID` is replaced by slug pinging, so both can be dropped.

## Development

```sh
make manifests generate   # regenerate CRDs, RBAC and deepcopy
make test                 # unit tests, no cluster needed
make test-crd             # CRD validation against a real API server (envtest)
make test-e2e             # full deploy against a throwaway Kind cluster
make build-installer IMG=...   # render dist/install.yaml
```

Most of the validation lives in CEL rules on the CRDs, which no Go test can
reach: a rule that fails to compile makes every CRD install fail, and one that
never fires is invisible. `make test-crd` runs them against a real API server.

Releases publish a container image to `ghcr.io/clevyr/borgbase-operator` and
the rendered manifests as an OCI artifact at
`ghcr.io/clevyr/borgbase-operator-manifests`, which Flux consumes via an
`OCIRepository`.

## corg, the CLI

`corg` runs backups on demand, explains why one is not running, and restores
snapshots. It is the same binary as the `kubectl corg` plugin.

See [docs/corg.md](docs/corg.md) for the full command reference.

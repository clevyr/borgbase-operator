# corg

Operating the backups that `borgbase-operator` schedules.

The same binary works either way:

```
kubectl corg <command>    # installed as a kubectl plugin
corg <command>            # standalone
```

## What a command does to your data

Every command below is marked. It is the first thing to check at 3am.

| Marker | Meaning |
| --- | --- |
| `reads` | Changes nothing. Safe against production while you think. |
| `writes` | Alters Kubernetes objects — a schedule, an annotation, a Job. Reversible. |
| `DESTROYS` | Overwrites or deletes what is being protected. Asks you to type the resource name, and refuses outright with no terminal unless `--yes` is passed. |

## Naming what you mean

A bare name matching both a ScheduledBackup and a Repository is refused rather
than guessed at.

| Form | Resolves to |
| --- | --- |
| `web-files` | Whichever kind has that name — an error if both do |
| `sb/web-files` | A ScheduledBackup. Also `scheduledbackup/`, `backup/` |
| `repo/prod` | A Repository. Also `repository/` |

Standard kubectl connection flags work everywhere: `-n/--namespace`,
`--context`, `--kubeconfig`. `-A` and `-o` appear only on `get`, where they
mean something.

## Inspect

### `doctor [name]` — `reads`

**Start here when something is wrong.** Checks the whole chain in one pass:
credentials Secret, CronJob ownership, repository initialization, cache claim,
and whether the most recent run actually succeeded. Names the object to fix and
the command to run next. With no argument, checks every resource in the
namespace. Exits non-zero if any check fails.

### `get [repositories|backups]` — `reads`

Both kinds by default. Pairs usage against quota, and last-success against
whether the schedule is suspended, so "are backups happening" is one command.

| Flag | |
| --- | --- |
| `-A` | Every namespace |
| `-o wide` | Adds server, secret, timezone, concurrency policy |
| `-o json\|yaml\|name` | Machine-readable |

### `status <name>` — `reads`

One resource in detail: usage against quota, the **effective** schedule with its
timezone, conditions with reasons, and a table of recent runs with result and
duration.

| Flag | |
| --- | --- |
| `--limit` | Runs to show (default 10) |

### `logs <name>` — `reads`

The most recent run's output. Finds runs by ownership, so triggered and
scheduled runs both appear. Jobs are deleted an hour after they finish; past
that, `status` still has the history.

| Flag | |
| --- | --- |
| `-f` | Follow |
| `-p` | The run before the most recent |
| `--tail` | Trailing lines only |

## Run

### `backup <name>` — `writes` — alias `run`

**Runs even when the schedule is suspended** — suspending and then backing up by
hand is the main reason to suspend. Refused only if a run is already in flight
and `concurrencyPolicy: Forbid`. A backup that is not Ready reports that the run
is deferred rather than appearing to hang.

| Flag | |
| --- | --- |
| `--wait` | Block until it finishes; non-zero if it fails |
| `-f` | Stream the logs (implies `--wait`) |
| `--timeout` | Default 2h |

Equivalent without the CLI:

```sh
kubectl annotate scheduledbackup/web-files \
  borgbase.clevyr.com/trigger-at="$(date -Is)" --overwrite
```

### `cancel <name>` — `writes`

Deletes the Job of a run in flight, leaving completed ones for history. A killed
restic leaves a lock behind; `unlock` clears it.

### `wait <name>` — `reads`

Blocks until the current run finishes. Exits non-zero on failure, so it can gate
a deploy or a migration.

## Snapshots and restore

### `snapshots <name>` — `reads` — alias `snaps`

The backup's own snapshots. One repository can serve several backups, so the
listing is filtered to this backup's source tags. Host defaults to the
namespace, which is what the backup records them under.

| Flag | |
| --- | --- |
| `--all-tags` | The whole repository |
| `--host` | Override; empty string lifts the filter |
| `--latest` | Newest N only |
| `--json` | restic's JSON output |

### `restore <name>` — `DESTROYS`

Four destinations. **With a terminal and no target, corg asks**, offering only
what this backup can do. Without a terminal it lists them and exits, so a script
never restores somewhere it did not name.

| Target | | |
| --- | --- | --- |
| `--to DIR` | `reads` | Streams the files to this machine. Nothing staged on disk at either end |
| `--to-new-pvc NAME` | `writes` | A fresh claim, sized from the source, to inspect before committing |
| `--in-place` | `DESTROYS` | Over the source volume. Suspends the schedule for the duration |
| `--to-database` | `DESTROYS` | Streams the dump back through `restoredb` |

| Flag | |
| --- | --- |
| `--snapshot` | Default `latest` |
| `--path`, `--include`, `--exclude` | Restore part of a snapshot |
| `--delete` | Remove files at the target that are not in the snapshot |
| `--size` | Override the new claim's size |
| `--dry-run` | Report without writing |
| `--yes` | Skip the typed confirmation |

## Maintenance

### `unlock <name>` — `writes`

**The usual 3am fix.** A backup killed mid-run leaves a lock, and every later run
fails waiting on it. Removes only what restic considers stale.

| Flag | |
| --- | --- |
| `--remove-all` | **`DESTROYS`** Every lock, stale or not. Only safe when nothing is running. Confirms first |
| `--yes` | Skip the confirmation |

### `check <name>` — `reads`

Verifies repository structure. Metadata only by default, which is fast.

| Flag | |
| --- | --- |
| `--read-data-subset` | Also re-read a share of the data (`10%`, `1/5`). The only way to catch bit rot, at the cost of downloading that share |

### `prune <name>` — `DESTROYS`

Applies the ScheduledBackup's own retention policy, so a manual prune does what
the scheduled one does. Backups already prune themselves after each run; this is
for reclaiming space after a retention change. Refuses on an append-only
repository.

| Flag | |
| --- | --- |
| `--dry-run` | What would be removed. Skips the confirmation |
| `--yes` | Skip the confirmation |

### `stats <name>` — `reads`

restic's own view of repository size, which differs from BorgBase's reported
usage in `status` because of deduplication and compression.

| Flag | |
| --- | --- |
| `--mode` | `restore-size` (default), `raw-data`, `files-by-contents`, `blobs-per-file` |

## Lifecycle

### `suspend <name>` / `resume <name>` — `writes`

Sets or clears `spec.suspend`. On a ScheduledBackup the CronJob is kept but stops
firing; on a Repository the operator stops reconciling it entirely.
Re-suspending is a no-op, not a pointless write.

### `reinit <repository>` — `writes`

The init Job retries on a five-minute delay and then stops. If it failed for a
reason since fixed, this clears the recorded state and the failed Job so the
operator starts over. Does not touch repository contents: `restic init` on an
existing repository is a no-op.

### `rotate-password <repository>` — `DESTROYS`

**Adds** a key rather than replacing one, so snapshots from before the rotation
stay readable and the change is undoable by restoring the old Secret. Remove the
old key yourself once you are sure:

```sh
corg exec web-files -- restic key list
corg exec web-files -- restic key remove <id>
```

Requires `--i-have-somewhere-to-archive-the-new-password`. The password is the
only thing that can decrypt this repository.

## Escape hatch

The pod is deleted when you exit. The app's data is not mounted unless you ask,
and the shared restic cache is replaced with scratch space, so none of this can
interfere with a running backup.

### `shell <name>` — `writes` — alias `sh`

A pod from the backup image with `restic`, `runitor`, `ts` and `dumpdb` on PATH
and the credentials already in the environment, so `restic snapshots` just
works — and so does `restic mount /mnt`.

| Flag | |
| --- | --- |
| `--mount-data` | **`DESTROYS`** Mount the source volume, writable |
| `--mount-cache` | Use the shared cache instead of scratch space |
| `--image` | Override the image |
| `--keep` | Leave the pod running |
| `--shell` | Default `sh` |

### `exec <name> -- <command>...` — `writes`

One command in the same environment, output printed. Reaches restic subcommands
the CLI does not wrap.

```sh
corg exec web-files -- restic find --tag=files 'config.php'
corg exec web-files -- restic cat config
```

### `env <name>` — `reads`

The credentials as shell exports. Redacted by default — note that
`RESTIC_REPOSITORY` embeds the password too, so it is redacted alongside. The
notice goes to stderr, so stdout stays evaluable.

```sh
eval "$(corg env repo/prod --show-password)"
```

## Development

### `render <file.yaml|name>` — `reads`

The exact shell script a backup runs. Takes a local manifest or a resource in
the cluster, so it answers both "what would this change do" and "what is this
running". `--cronjob` prints the whole object instead.

### `validate <file.yaml>` — `reads`

Catches what the operator would reject at reconcile time — an unresolvable
schedule, a source it cannot render, a missing database field — with no cluster.
Does not run the CRD's CEL rules; the API server owns those.

### `parity <generated.yaml> <helmrelease.yaml>` — `reads`

Confirms that migrating an app does not change what gets backed up. Needs `yq`
on PATH.

**Schedules are compared by cadence, not verbatim.** Migration hands a
hand-jittered expression back to the operator as a shorthand, which deliberately
moves the minute. Both expressions are expanded over 28 days and checked for
firing equally often with the same spacing, so "hourly became daily" fails while
a re-jittered minute does not.

| Output | Exit | |
| --- | --- | --- |
| `IDENTICAL` | 0 | Nothing moved |
| `RESCHEDULED` + `EQUIVALENT` | 0 | The time moved, the cadence did not |
| `SCRIPT DIFFERS` | 1 | |
| `CADENCE DIFFERS` | 1 | It no longer runs as often |
| `TIMEZONE DIFFERS` | 1 | |

### `migrate <resources/restic directory>` — `reads`

Converts an app's hand-written backup into operator resources, printed to
stdout. Nothing is applied and nothing in fleet-infra is modified.

The resources are marshalled from the real API types, so the output cannot be
shaped in a way the CRDs would reject.

| Flag | |
| --- | --- |
| `--repository-id` | Use this ID instead of decrypting `secret.yaml` |

The BorgBase repository ID exists only inside the encrypted
`RESTIC_REPOSITORY` value, so by default `migrate` runs `sops` to read it —
that is where the cloud credentials for it live, and linking the sops library
instead would add every KMS backend it supports (61MB on a 93MB binary) to read
one field once per app. `--repository-id` skips that path entirely. Everything
else is parsed and emitted in Go.

Before applying, reduce the app's `secret.yaml` to just `RESTIC_PASSWORD`:
`RESTIC_REPOSITORY` is synthesized by the operator and `CHECK_UUID` is replaced
by healthchecks slug pinging. Do not remove `RESTIC_PASSWORD` — it is the only
off-cluster copy of the key that decrypts these backups.

## Recipes

**A backup stopped working**

```sh
corg doctor web-files
# fix what it names, then prove it
corg backup web-files --follow
```

**Last night's run failed**

```sh
corg status web-files   # when, and how often
corg logs web-files     # why
corg unlock web-files   # if it died mid-run
```

**Recover one file**

```sh
corg snapshots web-files
corg restore web-files --snapshot 4f2a1b0c \
  --path app/config.php --to ./recovered
```

**Full recovery, carefully**

```sh
# stage it first and look at it
corg restore web-files --to-new-pvc web-files-check
corg shell web-files --mount-data
# only then, over the live volume
corg restore web-files --in-place
```

**Quiet a backup during maintenance**

```sh
corg suspend web-files
# … do the work …
corg backup web-files --wait
corg resume web-files
```

**Migrate an app onto the operator**

```sh
corg migrate apps/snipe-it/resources/restic > generated.yaml
corg validate generated.yaml
corg parity generated.yaml helmrelease.yaml
```

## Installing

```sh
brew install clevyr/tap/corg
```

The formula symlinks `kubectl-corg` alongside `corg`, so it works as a plugin
with no extra step.

### Permissions

Apply `config/rbac/corg_user_role.yaml` and bind one of:

- **`corg-viewer`** — read-only. Cannot read the credentials Secret.
- **`corg-operator`** — runs backups and restores. **Reads and writes the
  credentials Secret**, which holds the key that decrypts every snapshot. Bind
  it only to people who should hold that key.

The operator's own role does not cover the CLI: it never reads pods, never
execs, and cannot delete a claim.

## After a migration

Two things change on cutover, and neither is a fault:

- `spec.schedule` becomes a shorthand such as `@hourly`, and the real cron
  expression exists only in `status.effectiveSchedule`. `get` and `status` show
  the effective one.
- A backup whose new slot has already passed waits for the next one, so a daily
  backup can go up to a day longer before its first run. `doctor` reports this
  as "no backup has run yet", not as a missed backup. Take one now with
  `corg backup <name>`.

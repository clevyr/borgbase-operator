# borgbase-operator - AI Agent Guide

A Kubernetes operator that creates and adopts BorgBase restic repositories and
renders the CronJobs that back up into them. Read the README first: it explains
what the two resources are for and why the defaults are what they are.

## Layout

```
cmd/main.go                    Manager entry and operator-level flags
api/v1/*_types.go              CRD schemas (+kubebuilder markers)
api/v1/zz_generated.*          Auto-generated (DO NOT EDIT)
internal/controller/           Reconciliation for both kinds
internal/backup/               Rendering: CronJob, schedule, backup script
internal/borgbase/             GraphQL client for the BorgBase API
internal/healthchecks/         runitor wrapping, no API client of its own
internal/secrets/              Restic password generation
config/crd/bases/              Generated CRDs (DO NOT EDIT)
config/rbac/role.yaml          Generated RBAC (DO NOT EDIT)
config/samples/                Example CRs, checked by make test-crd
hack/migrate.sh                Generate resources from a hand-written backup
hack/parity/                   Compare generated output against the original
PROJECT                        Kubebuilder metadata (DO NOT EDIT)
```

There is no webhook and no multi-group layout. Both kinds live in `api/v1`.

## What this code is protecting

Nearly every non-obvious choice here exists to avoid destroying or orphaning
backups. Before changing something that looks redundant, check whether it is
one of these:

- **The restic password is generated once and never rotated.** It is the
  encryption key for every snapshot. The controller refuses to invent one for a
  repository that has been recorded, initialized, or reports any usage.
- **Adoption never creates.** A wrong `existingRepositoryID` must fail loudly
  rather than provision an empty repository beside the real backups.
- **`deletionPolicy` defaults to Retain**, and the credentials Secret only gets
  an ownerReference under `Delete`.
- **The operator only writes to Secrets it created**, identified by the
  `app.kubernetes.io/managed-by` label.
- **The API token is resolved lazily**, so a Retain deletion works without one.
- **Status is written with a merge patch**, never a full update: the informer
  cache lags behind the controller's own writes.
- **The init Job is deleted a pass after success is recorded**, not in the same
  one, or the watch event races the status write.

## Generated files

After editing `*_types.go` or any marker:

```sh
make manifests generate
```

Never hand-edit `config/crd/bases/*`, `config/rbac/role.yaml` or
`**/zz_generated.*`. Do not delete `// +kubebuilder:scaffold:*` comments.

## Tests

```sh
make test        # unit tests: plain go test, no cluster, no envtest binaries
make test-crd    # CRD validation against a real API server (envtest)
make test-e2e    # full deploy against a throwaway Kind cluster
make lint        # golangci-lint with the logcheck plugin
```

Tests are standard `testing`, not Ginkgo, except the e2e suite. There is no
`suite_test.go` and unit tests do not use envtest.

**Validation lives in CEL on the CRDs**, so `make test` cannot see it. A rule
that fails to compile makes every CRD install fail, and one that never fires is
invisible. Anything added under `+kubebuilder:validation:XValidation` needs a
case in `internal/controller/crd_validation_envtest_test.go`.

CEL gotcha: `\.` is not a valid escape in a CEL string literal. Use a character
class, `[.]`, instead.

## Migration

`hack/migrate.sh` reads an app's hand-written `resources/restic` directory and
emits the two resources, converting a hand-jittered cron expression into the
equivalent shorthand so the operator can jitter it instead.

`hack/parity` then compares the generated script, schedule and time zone
against the original. Migrating must not silently change what gets backed up or
how often, so any change to `internal/backup/script.go` or `schedule.go` has to
keep parity meaningful. It normalises away only the quoting around `--exclude`
patterns and the `--retry-lock` flag, and compares the schedule by **cadence**,
expanding both expressions over 28 days and comparing how often they fire.
Comparing schedules verbatim would report a difference for every app, since
migration moves the time on purpose, and hide the ones that matter.

## Logging

Kubernetes conventions, enforced by the logcheck linter: start with a capital,
no trailing period, past tense, name the object type, balanced key-value pairs.

```go
log.Info("Created Deployment", "name", deploy.Name)
log.Error(err, "Failed to create Pod", "name", name)
```

## References

- Kubebuilder Book: https://book.kubebuilder.io
- controller-runtime FAQ: https://github.com/kubernetes-sigs/controller-runtime/blob/main/FAQ.md
- Logging conventions: https://github.com/kubernetes/community/blob/master/contributors/devel/sig-instrumentation/logging.md#message-style-guidelines
- CEL validation rules: https://kubernetes.io/docs/tasks/extend-kubernetes/custom-resources/custom-resource-definitions/#validation-rules

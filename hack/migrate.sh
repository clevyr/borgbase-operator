#!/usr/bin/env bash
#
# Generate borgbase-operator resources from an existing hand-written restic
# backup in fleet-infra.
#
# Output is written to stdout for review. Nothing is applied and nothing in
# fleet-infra is modified: the generated resources pin the current schedule,
# retention and sources verbatim, and a human should diff them against the
# HelmRelease before committing.
#
# Usage:
#   hack/migrate.sh <path to an app's resources/restic directory>
#
# Example:
#   hack/migrate.sh ../fleet-infra/apps/fennec/myapp/prod/resources/restic
set -euo pipefail

dir=${1:?usage: migrate.sh <resources/restic directory>}
hr=$dir/helmrelease.yaml
secret=$dir/secret.yaml

[[ -f $hr ]] || { echo "no helmrelease.yaml in $dir" >&2; exit 1; }
[[ -f $secret ]] || { echo "no secret.yaml in $dir" >&2; exit 1; }

for tool in yq sops; do
  command -v "$tool" >/dev/null || { echo "$tool is required" >&2; exit 1; }
done

namespace=$(yq -r '.metadata.namespace' "$hr")

# The BorgBase repository ID is only recorded inside the encrypted
# RESTIC_REPOSITORY value, in the form rest:https://<id>:<pass>@<id>.repo...
repo_url=$(sops --decrypt "$secret" | yq -r '.stringData.RESTIC_REPOSITORY')
repo_id=${repo_url#rest:https://}
repo_id=${repo_id%%:*}
[[ $repo_id =~ ^[a-z0-9]+$ ]] || { echo "could not parse a repository id from $secret" >&2; exit 1; }

values=$(yq -r '.spec.values' "$hr")
get() { printf '%s' "$values" | yq -r "$1"; }

schedule=$(get '.controllers.restic.cronjob.schedule')

# The hand-written schedules are hand-jittered: someone picked a minute to keep
# apps off the top of the hour, and copy-paste means several apps ended up
# sharing one. Hand them back to the operator as a cadence, which it jitters
# from a hash of the resource, so the spread is maintained without anyone
# maintaining it. Only the shapes that map exactly are converted; anything else
# is pinned verbatim, since a schedule nobody can read is not worth guessing at.
shorthand=$(awk '{
  if (NF != 5) exit
  minute = $1; hour = $2
  if ($3 != "*" || $4 != "*" || $5 != "*") exit
  if (minute !~ /^[0-9]+$/) exit
  if (hour == "*") { print "@hourly"; exit }
  if (hour ~ /^[0-9]+$/) { print "@daily"; exit }
  if (hour ~ /^\*\/[0-9]+$/) { sub(/^\*\//, "", hour); print "@every " hour "h"; exit }
}' <<<"$schedule")
concurrency=$(get '.controllers.restic.cronjob.concurrencyPolicy // "Forbid"')
# The CronJob's time zone decides what the schedule actually means, whether it
# was pinned or handed back as a shorthand. The CRD defaults it to
# America/Chicago, so an app on anything else has to carry it across or every
# backup silently moves.
timezone=$(get '.controllers.restic.cronjob.timeZone // ""')
script=$(get '.controllers.restic.containers.restic.command[-1]')
workdir=$(get '.controllers.restic.containers.restic.workingDir // ""')
claim=$(get '[.persistence[] | select(has("existingClaim")) | .existingClaim][0] // ""')
db_secret=$(get '[.persistence | to_entries[] | select(.value.type == "secret") | .value.name][0] // ""')
db_host=$(get '.controllers.restic.containers.restic.env.DB_HOST // ""')
db_name=$(get '.controllers.restic.containers.restic.env.DB_DATABASE // ""')
db_user=$(get '.controllers.restic.containers.restic.env.DB_USERNAME // ""')
cache_class=$(get '.persistence.cache.storageClass // ""')

# Retention comes out of the `restic forget` line as --keep-<unit>=<n> flags.
retention=$(grep -o -- '--keep-[a-z]*=[0-9]*' <<<"$script" | sed 's/--keep-//' \
  | awk -F= '{printf "    %s: %s\n", $1, $2}')

engine=""
[[ $script == *"dumpdb cnpg"* ]] && engine=cnpg
[[ $script == *"dumpdb mariadb"* ]] && engine=mariadb

emit_sources() {
  # Database sources, in the order they appear in the script.
  while read -r line; do
    [[ -n $line ]] || continue
    local tag db
    tag=$(grep -o -- '--tag=[^ ]*' <<<"$line" | cut -d= -f2)
    db=$(grep -o -- '--database=[^ ]*' <<<"$line" | cut -d= -f2 || true)
    printf '    - type: %s\n      tag: %s\n' "$engine" "$tag"
    [[ -n $db ]] && printf '      database: %s\n' "$db"
    [[ $line == *"-- --skip-ssl"* ]] && printf '      extraArgs: ["--skip-ssl"]\n'
  done < <(grep -- '--stdin-from-command' <<<"$script" || true)

  # A single files source, with its path and excludes.
  local files_line
  files_line=$(grep -- '--tag=files' <<<"$script" | grep -v 'stdin-from-command' || true)
  if [[ -n $files_line ]]; then
    local tag path
    tag=$(grep -o -- '--tag=[^ ]*' <<<"$files_line" | cut -d= -f2)
    path=$(sed -E 's/.*--tag=[^ ]+ +([^ \\]+).*/\1/' <<<"$files_line")
    printf '    - type: files\n      tag: %s\n      path: %s\n' "$tag" "$path"
    # Excludes appear both single-quoted ('**/temp*') and bare (dumps), so
    # match either form. Missing one would silently widen what gets backed up.
    local excludes found expected
    excludes=$(grep -oE -- "--exclude=('[^']*'|[^ \\\\]+)" <<<"$script" \
      | sed "s/^--exclude=//; s/^'//; s/'$//" || true)
    expected=$(grep -c -o -- '--exclude=' <<<"$script" || true)
    found=$(grep -c . <<<"$excludes" || true)
    if [[ ${expected:-0} -ne ${found:-0} ]]; then
      echo "migrate.sh: parsed $found of $expected --exclude patterns in $hr; refusing to emit a partial list" >&2
      exit 1
    fi
    if [[ -n $excludes ]]; then
      printf '      exclude:\n'
      while read -r ex; do [[ -n $ex ]] && printf '        - %s\n' "\"$ex\""; done <<<"$excludes"
    fi
  fi
}

cat <<EOF
# Generated by hack/migrate.sh from $hr
# Review against the HelmRelease before committing.
#
# Before applying, reduce $secret to just RESTIC_PASSWORD:
# RESTIC_REPOSITORY is synthesized by the operator, and CHECK_UUID is replaced
# by healthchecks slug pinging. Add PING_KEY for this client's healthchecks
# project. Do not remove RESTIC_PASSWORD: it is the only off-cluster copy of
# the key that decrypts these backups.
apiVersion: borgbase.clevyr.com/v1
kind: Repository
metadata:
  name: restic
  namespace: $namespace
spec:
  existingRepositoryID: $repo_id
  passwordSecretRef:
    name: restic-envs
    key: RESTIC_PASSWORD
---
apiVersion: borgbase.clevyr.com/v1
kind: ScheduledBackup
metadata:
  name: restic
  namespace: $namespace
spec:
  repositoryRef:
    name: restic
$(if [[ -n $shorthand ]]; then
  printf '  # Was "%s" in the HelmRelease, hand-jittered. The operator derives\n' "$schedule"
  printf '  # its own minute from a hash of this resource, so the spread is kept\n'
  printf '  # without anyone maintaining it. Check status.effectiveSchedule.\n'
  printf '  schedule: "%s"' "$shorthand"
else
  printf '  # Pinned verbatim: this schedule has no shorthand equivalent.\n'
  printf '  schedule: "%s"' "$schedule"
fi)
  concurrencyPolicy: $concurrency
  sources:
$(emit_sources)
  retention:
$retention
EOF

if [[ -n $timezone ]]; then
  printf '  timeZone: %s\n' "$timezone"
fi

if [[ -n $engine ]]; then
  printf '  database:\n    engine: %s\n' "$engine"
  [[ -n $db_secret ]] && printf '    secretName: %s\n' "$db_secret"
  [[ -n $db_host ]] && printf '    host: %s\n' "$db_host"
  [[ -n $db_name ]] && printf '    name: %s\n' "$db_name"
  [[ -n $db_user ]] && printf '    user: %s\n' "$db_user"
fi

if [[ -n $claim ]]; then
  printf '  volume:\n    existingClaim: %s\n' "$claim"
  # workingDir defaults to /<claim>; only emit a mountPath if it differed.
  [[ -n $workdir && $workdir != "/$claim" ]] && printf '    mountPath: %s\n' "$workdir"
fi

[[ -n $cache_class ]] && printf '  cache:\n    storageClass: %s\n' "$cache_class"

cat <<'EOF'
  healthchecks:
    pingKeySecretRef:
      name: healthchecks-ping-key
      key: PING_KEY
EOF

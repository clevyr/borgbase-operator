// Command parity compares what this operator would generate against the
// hand-written HelmRelease it replaces.
//
// Migrating an app must not silently change what gets backed up, nor when, so
// run this for each app before cutting it over and account for every
// difference.
//
// Usage:
//
//	go run ./hack/parity <generated.yaml> <helmrelease.yaml>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"sigs.k8s.io/yaml"
)

// defaultTimeZone is the CRD's default, and therefore what an omitted
// spec.timeZone resolves to.
const defaultTimeZone = "America/Chicago"

// plan is what a backup will do: the script, and when it runs.
type plan struct {
	script   string
	schedule string
	timeZone string
}

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: parity <generated.yaml> <helmrelease.yaml>")
		os.Exit(2)
	}

	rendered, err := renderGenerated(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rendering generated resource:", err)
		os.Exit(1)
	}
	original, err := originalPlan(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading original backup:", err)
		os.Exit(1)
	}

	var differences, notes []string
	if normalize(rendered.script) != normalize(original.script) {
		differences = append(differences, "SCRIPT DIFFERS")
	}

	// The schedule is compared by cadence, not by the minute it lands on.
	// Migration hands a hand-jittered expression back to the operator as a
	// shorthand, which deliberately moves the time; what must not change is how
	// often the backup runs, since that is what the retention tiers assume.
	renderedCadence, err := cadenceOf(rendered.schedule)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading generated schedule:", err)
		os.Exit(1)
	}
	originalCadence, err := cadenceOf(original.schedule)
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading original schedule:", err)
		os.Exit(1)
	}
	switch {
	case !renderedCadence.equal(originalCadence):
		differences = append(differences, fmt.Sprintf(
			"CADENCE DIFFERS: original %q runs %s, generated %q runs %s",
			original.schedule, originalCadence, rendered.schedule, renderedCadence))
	case rendered.schedule != original.schedule:
		notes = append(notes, fmt.Sprintf(
			"RESCHEDULED: %q -> %q (%s either way; the operator jittered it)",
			original.schedule, rendered.schedule, renderedCadence))
	}

	if rendered.timeZone != original.timeZone {
		differences = append(differences, fmt.Sprintf(
			"TIMEZONE DIFFERS: original %q, generated %q", original.timeZone, rendered.timeZone))
	}

	for _, n := range notes {
		fmt.Println(n)
	}
	if len(differences) == 0 {
		if len(notes) == 0 {
			fmt.Println("IDENTICAL")
		} else {
			fmt.Println("EQUIVALENT")
		}
		return
	}

	for _, d := range differences {
		fmt.Println(d)
	}
	if normalize(rendered.script) != normalize(original.script) {
		fmt.Println("--- original")
		fmt.Println(original.script)
		fmt.Println("--- rendered")
		fmt.Println(rendered.script)
	}
	os.Exit(1)
}

func renderGenerated(path string) (plan, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return plan{}, err
	}
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if !strings.Contains(doc, "kind: ScheduledBackup") {
			continue
		}
		var sb borgbasev1.ScheduledBackup
		if err := yaml.Unmarshal([]byte(doc), &sb); err != nil {
			return plan{}, err
		}
		script, err := backup.Render(&sb.Spec)
		if err != nil {
			return plan{}, err
		}
		// Resolved rather than taken verbatim, because a shorthand schedule is
		// jittered and the comparison has to see what the CronJob will get.
		schedule, err := backup.ResolveSchedule(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name)
		if err != nil {
			return plan{}, err
		}
		tz := sb.Spec.TimeZone
		if tz == "" {
			tz = defaultTimeZone
		}
		return plan{script: script, schedule: schedule, timeZone: tz}, nil
	}
	return plan{}, fmt.Errorf("no ScheduledBackup document in %s", path)
}

// originalPlan pulls the shell body, schedule and time zone out of the
// HelmRelease. The script is the last element of the runitor invocation.
func originalPlan(path string) (plan, error) {
	script, err := yq(path, ".spec.values.controllers.restic.containers.restic.command[-1]")
	if err != nil {
		return plan{}, err
	}
	schedule, err := yq(path, ".spec.values.controllers.restic.cronjob.schedule")
	if err != nil {
		return plan{}, err
	}
	tz, err := yq(path, `.spec.values.controllers.restic.cronjob.timeZone // ""`)
	if err != nil {
		return plan{}, err
	}
	if tz == "" {
		// An unset time zone in the HelmRelease means the cluster's, which for
		// this fleet is what the CRD defaults to.
		tz = defaultTimeZone
	}
	return plan{script: script, schedule: schedule, timeZone: tz}, nil
}

func yq(path, expr string) (string, error) {
	out, err := exec.Command("yq", "-r", expr, path).Output()
	if err != nil {
		return "", fmt.Errorf("running yq %q: %w", expr, err)
	}
	return strings.TrimRight(string(out), "\n"), nil
}

// normalize ignores differences that cannot change what gets backed up:
// trailing whitespace, the optional quoting around an --exclude pattern, and
// the --retry-lock flag.
//
// Quoting a bare pattern is a safety improvement, not a change in what it
// matches. --retry-lock only decides whether a command waits for the repository
// lock or fails immediately; it selects no different data. Both are deliberate
// improvements over the hand-written scripts, so comparing them verbatim would
// report a difference on every app and hide the ones that matter.
var retryLock = regexp.MustCompile(` --retry-lock=\S+`)

func normalize(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\\")
		line = retryLock.ReplaceAllString(line, "")
		// Only unquote exclude patterns; stripping a trailing quote from every
		// line could hide a genuine difference elsewhere in the script.
		if strings.Contains(line, "--exclude=") {
			line = strings.ReplaceAll(line, "--exclude='", "--exclude=")
			line = strings.TrimSuffix(strings.TrimRight(line, " \t"), "'")
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return strings.Join(lines, "\n")
}

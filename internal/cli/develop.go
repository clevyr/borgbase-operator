package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strings"

	"github.com/spf13/cobra"
	"sigs.k8s.io/yaml"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

const defaultTimeZone = "America/Chicago"

// Errors returned by the develop command.
var (
	ErrNoScheduledBackup = errors.New("no ScheduledBackup found")
	ErrParityDiffers     = errors.New("the generated backup differs from the original")
)

func newRenderCommand(f *Factory) *cobra.Command {
	var asCronJob bool

	cmd := &cobra.Command{
		Use:     "render <file.yaml|name>",
		Short:   "Print the script a backup runs",
		GroupID: GroupDevelopment,
		Long: `Print the shell script a ScheduledBackup renders to.

Takes either a local manifest or the name of one in the cluster, so it answers
both "what would this change do" and "what is this actually running".`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := loadScheduledBackup(cmd.Context(), f, args[0])
			if err != nil {
				return err
			}
			return renderBackup(f.Streams.Out, sb, asCronJob)
		},
	}

	cmd.Flags().BoolVar(&asCronJob, "cronjob", false, "Print the whole CronJob instead of the script")
	return cmd
}

func loadScheduledBackup(
	ctx context.Context, f *Factory, arg string,
) (*borgbasev1.ScheduledBackup, error) {
	if _, err := os.Stat(arg); err == nil {
		return readScheduledBackup(arg)
	}

	c, err := f.Client()
	if err != nil {
		return nil, err
	}
	ns, err := f.Namespace()
	if err != nil {
		return nil, err
	}
	target, err := Resolve(ctx, c, ns, arg)
	if err != nil {
		return nil, err
	}
	if target.Kind != TargetScheduledBackup {
		return nil, fmt.Errorf("render needs a ScheduledBackup, not a %s", target.Kind)
	}
	return target.ScheduledBackup, nil
}

func readScheduledBackup(path string) (*borgbasev1.ScheduledBackup, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if !strings.Contains(doc, "kind: ScheduledBackup") {
			continue
		}
		var sb borgbasev1.ScheduledBackup
		if err := yaml.Unmarshal([]byte(doc), &sb); err != nil {
			return nil, err
		}
		return &sb, nil
	}
	return nil, fmt.Errorf("%w in %s", ErrNoScheduledBackup, path)
}

func renderBackup(out io.Writer, sb *borgbasev1.ScheduledBackup, asCronJob bool) error {
	p := newPrinter(out)

	if !asCronJob {
		script, err := backup.Render(&sb.Spec)
		if err != nil {
			return err
		}
		p.println(script)
		return p.Err()
	}

	repo := &borgbasev1.Repository{
		Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name,
	}
	cj, err := backup.BuildCronJob(sb, repo, backup.Config{Image: "ghcr.io/clevyr/restic:latest"})
	if err != nil {
		return err
	}
	data, err := yaml.Marshal(cj)
	if err != nil {
		return err
	}
	p.print(string(data))
	return p.Err()
}

func newValidateCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "validate <file.yaml>",
		Short:   "Check a manifest without a cluster",
		GroupID: GroupDevelopment,
		Long: `Check that a ScheduledBackup manifest renders.

This catches what the operator would reject at reconcile time -- an
unresolvable schedule, a source it cannot render, a missing database field --
without needing a cluster. It does not run the CRD's CEL rules, which the API
server owns.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			sb, err := readScheduledBackup(args[0])
			if err != nil {
				return err
			}
			return validateBackup(f.Streams.Out, sb)
		},
	}
	return cmd
}

func validateBackup(out io.Writer, sb *borgbasev1.ScheduledBackup) error {
	p := newPrinter(out)

	schedule, err := backup.ResolveSchedule(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name)
	if err != nil {
		return fmt.Errorf("spec.schedule: %w", err)
	}
	if _, err := backup.Render(&sb.Spec); err != nil {
		return fmt.Errorf("rendering the script: %w", err)
	}
	repo := &borgbasev1.Repository{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	if _, err := backup.BuildCronJob(sb, repo, backup.Config{Image: "placeholder"}); err != nil {
		return fmt.Errorf("building the CronJob: %w", err)
	}

	p.printf("scheduledbackup/%s is valid\n", sb.Name)
	p.printf("  schedule  %s\n", schedule)
	p.printf("  sources   %d\n", len(sb.Spec.Sources))
	return p.Err()
}

func newParityCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "parity <generated.yaml> <helmrelease.yaml>",
		Short:   "Compare a generated backup against the HelmRelease it replaces",
		GroupID: GroupDevelopment,
		Long: `Check that migrating an app does not change what gets backed up.

Compares the rendered script, the resolved schedule and the time zone against
the hand-written HelmRelease. Exits non-zero on any difference, so it can gate
a migration.

Requires yq on PATH to read the HelmRelease.`,
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runParity(f.Streams.Out, args[0], args[1])
		},
	}
	return cmd
}

type plan struct {
	script   string
	schedule string
	timeZone string
}

func runParity(out io.Writer, generatedPath, helmReleasePath string) error {
	rendered, err := renderedPlan(generatedPath)
	if err != nil {
		return fmt.Errorf("rendering the generated resource: %w", err)
	}
	original, err := originalPlan(helmReleasePath)
	if err != nil {
		return fmt.Errorf("reading the original backup: %w", err)
	}

	p := newPrinter(out)
	var differences, notes []string

	scriptDiffers := normalizeScript(rendered.script) != normalizeScript(original.script)
	if scriptDiffers {
		differences = append(differences, "SCRIPT DIFFERS")
	}

	renderedCadence, err := cadenceOf(rendered.schedule)
	if err != nil {
		return fmt.Errorf("reading the generated schedule: %w", err)
	}
	originalCadence, err := cadenceOf(original.schedule)
	if err != nil {
		return fmt.Errorf("reading the original schedule: %w", err)
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
		p.println(n)
	}
	if len(differences) == 0 {
		if len(notes) == 0 {
			p.println("IDENTICAL")
		} else {
			p.println("EQUIVALENT")
		}
		return p.Err()
	}

	for _, d := range differences {
		p.println(d)
	}
	if scriptDiffers {
		p.println("--- original")
		p.println(original.script)
		p.println("--- rendered")
		p.println(rendered.script)
	}
	if err := p.Err(); err != nil {
		return err
	}
	return ErrParityDiffers
}

func renderedPlan(path string) (plan, error) {
	sb, err := readScheduledBackup(path)
	if err != nil {
		return plan{}, err
	}
	script, err := backup.Render(&sb.Spec)
	if err != nil {
		return plan{}, err
	}

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

func originalPlan(path string) (plan, error) {
	hr, err := readHelmRelease(path)
	if err != nil {
		return plan{}, err
	}
	controller, container, err := hr.backupController()
	if err != nil {
		return plan{}, fmt.Errorf("%s: %w", path, err)
	}
	script, err := container.script()
	if err != nil {
		return plan{}, fmt.Errorf("%s: %w", path, err)
	}

	tz := controller.CronJob.TimeZone
	if tz == "" {
		tz = defaultTimeZone
	}
	return plan{script: script, schedule: controller.CronJob.Schedule, timeZone: tz}, nil
}

var ignoredFlags = regexp.MustCompile(` --(retry-lock|secret-mount)=\S+`)

func normalizeScript(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\\")
		line = ignoredFlags.ReplaceAllString(line, "")

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

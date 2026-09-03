package cli

import (
	"errors"
	"fmt"

	"github.com/spf13/cobra"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/cli/runner"
)

// ErrAppendOnly is returned when a prune is attempted against a repository that
// cannot accept one.
var ErrAppendOnly = errors.New("repository is append-only")

func newUnlockCommand(f *Factory) *cobra.Command {
	var removeAll, yes bool

	cmd := &cobra.Command{
		Use:     "unlock <name>",
		Short:   "Remove a stale restic lock",
		GroupID: GroupMaintenance,
		Long: `Remove stale locks from the repository.

A backup killed mid-run leaves a lock behind, and every later run then fails
waiting for it. This is the usual fix.

Only locks restic considers stale are removed. Use --remove-all to remove them
all, which is safe only when nothing is running.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// Removing a live lock while a backup holds it corrupts that run.
			if removeAll {
				if err := confirm(f, yes, "every lock on the repository, including live ones", args[0]); err != nil {
					return err
				}
			}
			return runRestic(cmd.Context(), f, args[0], "unlock",
				func(*borgbasev1.ScheduledBackup, *borgbasev1.Repository) ([]string, error) {
					argv := []string{"unlock"}
					if removeAll {
						argv = append(argv, "--remove-all")
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&removeAll, "remove-all", false,
		"Remove every lock, not only stale ones. Only safe when no backup is running")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

func newCheckCommand(f *Factory) *cobra.Command {
	var readData string

	cmd := &cobra.Command{
		Use:     "check <name>",
		Short:   "Verify repository integrity",
		GroupID: GroupMaintenance,
		Long: `Check the repository's structure.

By default only metadata is verified, which is fast. --read-data-subset also
re-reads a share of the actual data, which is the only way to detect bit rot
but costs a full download of that share.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestic(cmd.Context(), f, args[0], "check",
				func(*borgbasev1.ScheduledBackup, *borgbasev1.Repository) ([]string, error) {
					argv := []string{"check"}
					if readData != "" {
						argv = append(argv, "--read-data-subset="+readData)
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().StringVar(&readData, "read-data-subset", "",
		`Also verify a share of the data, e.g. "10%" or "1/5"`)
	return cmd
}

func newPruneCommand(f *Factory) *cobra.Command {
	var dryRun, yes bool

	cmd := &cobra.Command{
		Use:     "prune <name>",
		Short:   "Apply the retention policy and reclaim space",
		GroupID: GroupMaintenance,
		Long: `Run restic forget --prune using the ScheduledBackup's retention policy.

Backups already prune themselves after each run, so this is for reclaiming
space after a retention change rather than routine upkeep.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			// forget --prune deletes snapshots; there is no undo.
			if !dryRun {
				if err := confirm(f, yes, "snapshots outside the retention policy, permanently", args[0]); err != nil {
					return err
				}
			}
			return runRestic(cmd.Context(), f, args[0], "prune",
				func(sb *borgbasev1.ScheduledBackup, repo *borgbasev1.Repository) ([]string, error) {
					// restic cannot delete from an append-only repository, so
					// the run would fail well into the operation.
					if repo.Spec.AppendOnly {
						return nil, fmt.Errorf(
							"%w: repository/%s cannot forget or prune; clear spec.appendOnly first",
							ErrAppendOnly, repo.Name)
					}

					flags := retentionFlags(sb.Spec.Retention)
					if len(flags) == 0 {
						return nil, fmt.Errorf(
							"scheduledbackup/%s has no spec.retention, so there is nothing to forget",
							sb.Name)
					}

					argv := append([]string{"forget", "--prune"}, flags...)
					if dryRun {
						argv = append(argv, "--dry-run")
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be removed without removing it")
	cmd.Flags().BoolVar(&yes, "yes", false, "Skip the confirmation prompt")
	return cmd
}

// retentionFlags mirrors the flags the backup script uses, so a manual prune
// applies exactly the policy the scheduled one does.
func retentionFlags(r *borgbasev1.Retention) []string {
	if r == nil {
		return nil
	}
	var flags []string
	for _, f := range []struct {
		name  string
		value *int32
	}{
		{"last", r.Last},
		{"hourly", r.Hourly},
		{"daily", r.Daily},
		{"weekly", r.Weekly},
		{"monthly", r.Monthly},
		{"yearly", r.Yearly},
	} {
		if f.value != nil {
			flags = append(flags, fmt.Sprintf("--keep-%s=%d", f.name, *f.value))
		}
	}
	return flags
}

func newStatsCommand(f *Factory) *cobra.Command {
	var mode string

	cmd := &cobra.Command{
		Use:     "stats <name>",
		Short:   "Show repository size",
		GroupID: GroupMaintenance,
		Args:    cobra.ExactArgs(1),
		Long: `Show restic's own view of the repository size.

BorgBase's reported usage is in ` + "`corg status`" + `; this is what restic
thinks, which differs because of deduplication and compression.`,
		RunE: func(cmd *cobra.Command, args []string) error {
			return runRestic(cmd.Context(), f, args[0], "stats",
				func(*borgbasev1.ScheduledBackup, *borgbasev1.Repository) ([]string, error) {
					return resticCommand("stats", "--mode="+mode), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().StringVar(&mode, "mode", "restore-size",
		"restic stats mode: restore-size, files-by-contents, blobs-per-file or raw-data")
	return cmd
}

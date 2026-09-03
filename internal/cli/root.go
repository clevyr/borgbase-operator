// Package cli implements corg, which doubles as the kubectl-corg plugin.
package cli

import (
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// Command groups, in the order they appear in help output.
const (
	GroupInspect     = "inspect"
	GroupRun         = "run"
	GroupSnapshots   = "snapshots"
	GroupMaintenance = "maintenance"
	GroupLifecycle   = "lifecycle"
	GroupEscape      = "escape"
	GroupDevelopment = "development"
)

const pluginPrefix = "kubectl-"

// DisplayName returns how the command should refer to itself, which differs when it is
// invoked as a kubectl plugin.
func DisplayName(argv0 string) string {
	base := filepath.Base(argv0)
	base = strings.TrimSuffix(base, ".exe")
	if name, ok := strings.CutPrefix(base, pluginPrefix); ok {
		return "kubectl " + strings.ReplaceAll(name, "_", "-")
	}
	return "corg"
}

// New returns the root command.
func New(streams genericiooptions.IOStreams, argv0 string) *cobra.Command {
	f := NewFactory(streams)

	display := DisplayName(argv0)
	name := display
	if after, ok := strings.CutPrefix(display, "kubectl "); ok {
		name = after
	}

	cmd := &cobra.Command{
		Use:   name,
		Short: "Operate borgbase-operator backups",
		Long: `corg drives the backups that borgbase-operator schedules.

It triggers backups on demand, explains why one is not running, browses and
restores snapshots, and drops you into a shell with restic already configured.`,
		SilenceUsage:  true,
		SilenceErrors: true,
		Annotations: map[string]string{
			cobra.CommandDisplayNameAnnotation: display,
		},
	}

	cmd.SetIn(streams.In)
	cmd.SetOut(streams.Out)
	cmd.SetErr(streams.ErrOut)

	f.ConfigFlags.AddFlags(cmd.PersistentFlags())

	subcommands := []*cobra.Command{
		newBackupCommand(f),
		newCancelCommand(f),
		newWaitCommand(f),
		newDoctorCommand(f),
		newGetCommand(f),
		newLogsCommand(f),
		newStatusCommand(f),
		newSuspendCommand(f),
		newResumeCommand(f),
		newReinitCommand(f),
		newRotatePasswordCommand(f),
		newEnvCommand(f),
		newSnapshotsCommand(f),
		newRestoreCommand(f),
		newUnlockCommand(f),
		newCheckCommand(f),
		newPruneCommand(f),
		newStatsCommand(f),
		newShellCommand(f),
		newExecCommand(f),
		newRenderCommand(f),
		newValidateCommand(f),
		newMigrateCommand(f),
		newParityCommand(f),
		newVersionCommand(f),
	}

	used := make(map[string]bool, len(subcommands))
	for _, sub := range subcommands {
		if sub.GroupID != "" {
			used[sub.GroupID] = true
		}
	}
	for _, g := range groupOrder() {
		if used[g.ID] {
			cmd.AddGroup(g)
		}
	}

	cmd.AddCommand(subcommands...)

	return cmd
}

func groupOrder() []*cobra.Group {
	return []*cobra.Group{
		{ID: GroupInspect, Title: "Inspect:"},
		{ID: GroupRun, Title: "Run:"},
		{ID: GroupSnapshots, Title: "Snapshots and restore:"},
		{ID: GroupMaintenance, Title: "Maintenance:"},
		{ID: GroupLifecycle, Title: "Lifecycle:"},
		{ID: GroupEscape, Title: "Escape hatch:"},
		{ID: GroupDevelopment, Title: "Development:"},
	}
}

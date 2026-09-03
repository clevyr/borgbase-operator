// Package cli implements corg, the companion CLI for the borgbase operator.
//
// The same binary serves as a standalone `corg` and as a kubectl plugin
// (`kubectl corg`), chosen by the name it was invoked under.
package cli

import (
	"path/filepath"
	"strings"

	"github.com/spf13/cobra"
	"k8s.io/cli-runtime/pkg/genericiooptions"
)

// Command group ids, declared here so every subcommand file can slot itself
// into the right section of `corg --help`.
const (
	GroupInspect     = "inspect"
	GroupRun         = "run"
	GroupSnapshots   = "snapshots"
	GroupMaintenance = "maintenance"
	GroupLifecycle   = "lifecycle"
	GroupEscape      = "escape"
	GroupDevelopment = "development"
)

// pluginPrefix is the kubectl plugin naming convention. kubectl also accepts an
// underscore in place of a dash for names that need one.
const pluginPrefix = "kubectl-"

// DisplayName derives how the binary should describe itself from argv[0], so
// help output and error messages match how the user actually invoked it.
func DisplayName(argv0 string) string {
	base := filepath.Base(argv0)
	base = strings.TrimSuffix(base, ".exe")
	if name, ok := strings.CutPrefix(base, pluginPrefix); ok {
		return "kubectl " + strings.ReplaceAll(name, "_", "-")
	}
	return "corg"
}

// New builds the root command. argv0 selects the standalone or plugin identity.
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

	// The standard kubectl connection flags, so -n/--context/--kubeconfig
	// behave exactly as they do everywhere else.
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
		newEnvCommand(f),
		newSnapshotsCommand(f),
		newUnlockCommand(f),
		newCheckCommand(f),
		newPruneCommand(f),
		newStatsCommand(f),
		newShellCommand(f),
		newExecCommand(f),
		newVersionCommand(f),
	}

	// Only register groups that something actually lands in, otherwise cobra
	// prints a bare heading with nothing under it.
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

// groupOrder is the order the sections appear in help output.
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

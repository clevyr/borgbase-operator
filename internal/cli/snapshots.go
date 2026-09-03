package cli

import (
	"fmt"

	"github.com/spf13/cobra"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/cli/runner"
)

func newSnapshotsCommand(f *Factory) *cobra.Command {
	var (
		allTags bool
		host    string
		asJSON  bool
		latest  int
	)

	cmd := &cobra.Command{
		Use:     "snapshots <name>",
		Aliases: []string{"snaps"},
		Short:   "List a backup's snapshots",
		GroupID: GroupSnapshots,
		Long: `List the snapshots a ScheduledBackup has written.

One repository can serve several backups, so the listing is filtered to this
backup's own source tags. Pass --all-tags to see the whole repository.

Snapshots are hosted by namespace, so the host filter defaults to the
namespace; --host="" lifts it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			if !cmd.Flags().Changed("host") {
				host = ns
			}

			return runRestic(cmd.Context(), f, args[0], "snapshots",
				func(sb *borgbasev1.ScheduledBackup, _ *borgbasev1.Repository) ([]string, error) {
					argv := []string{"snapshots"}
					if !allTags {
						for _, tag := range sourceTags(sb) {
							// Repeated --tag is an OR, which is what we want:
							// a backup's db and files snapshots are separate.
							argv = append(argv, "--tag="+tag)
						}
					}
					if host != "" {
						argv = append(argv, "--host="+host)
					}
					if asJSON {
						argv = append(argv, "--json")
					}
					if latest > 0 {
						argv = append(argv, fmt.Sprintf("--latest=%d", latest))
					}
					return resticCommand(argv...), nil
				},
				runner.Options{})
		},
	}

	cmd.Flags().BoolVar(&allTags, "all-tags", false, "Show every snapshot in the repository")
	cmd.Flags().StringVar(&host, "host", "", "Filter by snapshot host (defaults to the namespace)")
	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit restic's JSON output")
	cmd.Flags().IntVar(&latest, "latest", 0, "Show only the newest N snapshots")
	return cmd
}

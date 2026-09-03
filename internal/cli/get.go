package cli

import (
	"context"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func newGetCommand(f *Factory) *cobra.Command {
	var output string

	cmd := &cobra.Command{
		Use:     "get [repositories|backups]",
		Short:   "List repositories and scheduled backups",
		GroupID: GroupInspect,
		Long: `List the operator's resources.

With no argument both kinds are listed. Pass "repositories" or "backups" to
narrow it.`,
		Args:      cobra.MaximumNArgs(1),
		ValidArgs: []string{"repositories", "backups"},
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := ValidateOutput(output); err != nil {
				return err
			}

			kinds, err := kindsToList(args)
			if err != nil {
				return err
			}

			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.ListNamespace()
			if err != nil {
				return err
			}

			return runGet(cmd.Context(), c, f.Streams.Out, ns, f.AllNamespaces(), output, kinds)
		},
	}

	AddOutputFlag(cmd, &output)
	f.AddAllNamespacesFlag(cmd)
	return cmd
}

// kindsToList maps the optional positional argument to the kinds to render.
func kindsToList(args []string) ([]TargetKind, error) {
	if len(args) == 0 {
		return []TargetKind{TargetRepository, TargetScheduledBackup}, nil
	}
	kind, ok := kindAliases[args[0]]
	if !ok {
		return nil, fmt.Errorf("%w %q, expected \"repositories\" or \"backups\"", ErrUnknownKind, args[0])
	}
	return []TargetKind{kind}, nil
}

func runGet(
	ctx context.Context,
	c client.Client,
	out io.Writer,
	namespace string,
	allNamespaces bool,
	output string,
	kinds []TargetKind,
) error {
	var repos borgbasev1.RepositoryList
	var backups borgbasev1.ScheduledBackupList

	for _, kind := range kinds {
		var list client.ObjectList
		if kind == TargetRepository {
			list = &repos
		} else {
			list = &backups
		}
		if err := c.List(ctx, list, client.InNamespace(namespace)); err != nil {
			return err
		}
	}

	if IsMachineOutput(output) {
		return printGetObjects(out, output, kinds, &repos, &backups)
	}

	empty := true
	for _, kind := range kinds {
		var err error
		if kind == TargetRepository {
			if len(repos.Items) > 0 {
				empty = false
				err = printRepositories(out, repos.Items, allNamespaces, output)
			}
		} else {
			if len(backups.Items) > 0 {
				if !empty {
					fmt.Fprintln(out)
				}
				empty = false
				err = printScheduledBackups(out, backups.Items, allNamespaces, output)
			}
		}
		if err != nil {
			return err
		}
	}

	if empty {
		scope := fmt.Sprintf("in namespace %q", namespace)
		if allNamespaces {
			scope = "in any namespace"
		}
		fmt.Fprintf(out, "No resources found %s.\n", scope)
	}
	return nil
}

func printGetObjects(
	out io.Writer,
	output string,
	kinds []TargetKind,
	repos *borgbasev1.RepositoryList,
	backups *borgbasev1.ScheduledBackupList,
) error {
	for _, kind := range kinds {
		if kind == TargetRepository {
			repos.TypeMeta = metav1.TypeMeta{
				APIVersion: borgbasev1.SchemeGroupVersion.String(), Kind: "RepositoryList",
			}
			if err := PrintObject(out, output, repos); err != nil {
				return err
			}
			continue
		}
		backups.TypeMeta = metav1.TypeMeta{
			APIVersion: borgbasev1.SchemeGroupVersion.String(), Kind: "ScheduledBackupList",
		}
		if err := PrintObject(out, output, backups); err != nil {
			return err
		}
	}
	return nil
}

func printRepositories(out io.Writer, items []borgbasev1.Repository, allNamespaces bool, output string) error {
	if output == OutputName {
		for i := range items {
			fmt.Fprintf(out, "repository.borgbase.clevyr.com/%s\n", items[i].Name)
		}
		return nil
	}

	w := NewTabWriter(out)
	writeHeader(w, allNamespaces, output,
		[]string{"NAME", "REPO ID", "READY", "INITIALIZED", "USAGE", "QUOTA", "AGE"},
		[]string{"SERVER", "SECRET", "ADOPTED", "SUSPENDED"})

	for i := range items {
		r := &items[i]
		writeNamespace(w, allNamespaces, r.Namespace)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s",
			r.Name,
			orNone(r.Status.RepositoryID),
			ConditionStatus(r.Status.Conditions, borgbasev1.RepositoryConditionReady),
			yesNo(r.Status.Initialized),
			orNone(r.Status.CurrentUsage),
			orNone(r.Status.Quota),
			Age(r.CreationTimestamp),
		)
		if output == OutputWide {
			fmt.Fprintf(w, "\t%s\t%s\t%s\t%s",
				orNone(r.Status.Server),
				orNone(r.Status.SecretName),
				yesNo(r.Status.Adopted),
				yesNo(r.Spec.Suspend),
			)
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func printScheduledBackups(out io.Writer, items []borgbasev1.ScheduledBackup, allNamespaces bool, output string) error {
	if output == OutputName {
		for i := range items {
			fmt.Fprintf(out, "scheduledbackup.borgbase.clevyr.com/%s\n", items[i].Name)
		}
		return nil
	}

	w := NewTabWriter(out)
	writeHeader(w, allNamespaces, output,
		[]string{"NAME", "REPOSITORY", "SCHEDULE", "READY", "LAST BACKUP", "SUSPENDED", "ACTIVE", "AGE"},
		[]string{"TIMEZONE", "CONCURRENCY"})

	for i := range items {
		b := &items[i]
		writeNamespace(w, allNamespaces, b.Namespace)
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%d\t%s",
			b.Name,
			b.Spec.RepositoryRef.Name,
			orNone(b.Status.EffectiveSchedule),
			ConditionStatus(b.Status.Conditions, borgbasev1.ScheduledBackupConditionReady),
			Since(b.Status.LastSuccessfulTime),
			yesNo(b.Spec.Suspend),
			b.Status.Active,
			Age(b.CreationTimestamp),
		)
		if output == OutputWide {
			fmt.Fprintf(w, "\t%s\t%s", orNone(b.Spec.TimeZone), orNone(string(b.Spec.ConcurrencyPolicy)))
		}
		fmt.Fprintln(w)
	}
	return w.Flush()
}

func writeHeader(w io.Writer, allNamespaces bool, output string, cols, wideCols []string) {
	if allNamespaces {
		fmt.Fprint(w, "NAMESPACE\t")
	}
	for i, c := range cols {
		if i > 0 {
			fmt.Fprint(w, "\t")
		}
		fmt.Fprint(w, c)
	}
	if output == OutputWide {
		for _, c := range wideCols {
			fmt.Fprintf(w, "\t%s", c)
		}
	}
	fmt.Fprintln(w)
}

func writeNamespace(w io.Writer, allNamespaces bool, ns string) {
	if allNamespaces {
		fmt.Fprintf(w, "%s\t", ns)
	}
}

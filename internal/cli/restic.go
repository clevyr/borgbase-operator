package cli

import (
	"context"
	"errors"
	"fmt"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/cli/runner"
)

// defaultResticTimeout bounds an ephemeral run. A check or a prune over a large
// repository is slow, so this is generous.
const defaultResticTimeout = time.Hour

var ErrNoBackupForRepository = errors.New("no ScheduledBackup uses this repository")

// Runner builds a runner from the factory's clients.
func (f *Factory) Runner() (*runner.Runner, error) {
	c, err := f.Client()
	if err != nil {
		return nil, err
	}
	cs, err := f.Clientset()
	if err != nil {
		return nil, err
	}
	cfg, err := f.RESTConfig()
	if err != nil {
		return nil, err
	}
	return &runner.Runner{Client: c, Clientset: cs, RESTConfig: cfg}, nil
}

// resolveRunTarget finds the ScheduledBackup whose environment a restic command
// should borrow.
//
// Repository-scoped commands still need one: the credentials, image, and
// network placement all come from a backup's rendered CronJob, which is the
// only place the operator's resolved configuration exists.
func resolveRunTarget(
	ctx context.Context, c client.Client, namespace, arg string,
) (*borgbasev1.ScheduledBackup, *borgbasev1.Repository, error) {
	target, err := Resolve(ctx, c, namespace, arg)
	if err != nil {
		return nil, nil, err
	}

	if target.Kind == TargetScheduledBackup {
		repo, err := RepositoryFor(ctx, c, target.ScheduledBackup)
		if err != nil {
			return nil, nil, err
		}
		return target.ScheduledBackup, repo, nil
	}

	repo := target.Repository
	var backups borgbasev1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(repo.Namespace)); err != nil {
		return nil, nil, err
	}
	for i := range backups.Items {
		if backups.Items[i].Spec.RepositoryRef.Name == repo.Name {
			return &backups.Items[i], repo, nil
		}
	}
	return nil, nil, fmt.Errorf(
		"%w: repository/%s has no ScheduledBackup to borrow credentials and placement from",
		ErrNoBackupForRepository, repo.Name)
}

// sourceTags are the tags a ScheduledBackup writes, used to show only its own
// snapshots in a repository that may serve several backups.
func sourceTags(sb *borgbasev1.ScheduledBackup) []string {
	seen := map[string]bool{}
	var tags []string
	for i := range sb.Spec.Sources {
		tag := sb.Spec.Sources[i].EffectiveTag()
		if tag != "" && !seen[tag] {
			seen[tag] = true
			tags = append(tags, tag)
		}
	}
	return tags
}

// resticCommand builds the argv for a restic invocation inside the backup
// image. The image's restic wrapper is on PATH as `restic`.
func resticCommand(args ...string) []string {
	return append([]string{"restic"}, args...)
}

// runRestic is the shared path for every read-only restic subcommand.
func runRestic(
	ctx context.Context,
	f *Factory,
	arg string,
	purpose string,
	build func(sb *borgbasev1.ScheduledBackup, repo *borgbasev1.Repository) ([]string, error),
	opts runner.Options,
) error {
	c, err := f.Client()
	if err != nil {
		return err
	}
	ns, err := f.Namespace()
	if err != nil {
		return err
	}

	sb, repo, err := resolveRunTarget(ctx, c, ns, arg)
	if err != nil {
		return err
	}

	command, err := build(sb, repo)
	if err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	opts.Command = command
	opts.Purpose = purpose
	return run.Run(ctx, sb, opts, f.Streams.Out, defaultResticTimeout)
}

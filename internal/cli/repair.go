package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/cli/runner"
	"github.com/clevyr/borgbase-operator/internal/controller"
	"github.com/clevyr/borgbase-operator/internal/secrets"
)

// ErrNotInitialized means restic init has not run against the repository yet.
var ErrNotInitialized = errors.New("repository is not initialized")

func newReinitCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "reinit <repository>",
		Short:   "Re-run repository initialization",
		GroupID: GroupLifecycle,
		Long: `Make the operator run restic init again.

The init Job retries on a five minute delay and then stops. If it failed for a
reason since fixed -- a bad credential, a network policy -- this clears the
recorded state and the failed Job so the operator starts over.

This does not touch the repository's contents. restic init on a repository that
already exists is a no-op.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			target, err := Resolve(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			if target.Kind != TargetRepository {
				return fmt.Errorf("reinit needs a Repository, not a %s", target.Kind)
			}
			return runReinit(cmd.Context(), c, f.Streams.Out, target.Repository)
		},
	}
	return cmd
}

func runReinit(ctx context.Context, c client.Client, out io.Writer, repo *borgbasev1.Repository) error {
	p := newPrinter(out)

	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(repo.Namespace)); err != nil {
		return err
	}
	for i := range jobs.Items {
		if !metav1.IsControlledBy(&jobs.Items[i], repo) {
			continue
		}
		err := c.Delete(ctx, &jobs.Items[i],
			client.PropagationPolicy(metav1.DeletePropagationBackground))
		if err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting job/%s: %w", jobs.Items[i].Name, err)
		}
		p.printf("deleted job/%s\n", jobs.Items[i].Name)
	}

	patch := client.MergeFrom(repo.DeepCopy())
	repo.Status.Initialized = false
	if err := c.Status().Patch(ctx, repo, patch); err != nil {
		return fmt.Errorf("clearing status.initialized: %w", err)
	}

	p.printf("repository/%s will be initialized again\n", repo.Name)
	p.printf("  watch it with: corg doctor repo/%s\n", repo.Name)
	return p.Err()
}

func newRotatePasswordCommand(f *Factory) *cobra.Command {
	var archived bool

	cmd := &cobra.Command{
		Use:     "rotate-password <repository>",
		Short:   "Add a new restic encryption key",
		GroupID: GroupLifecycle,
		Long: `Rotate the restic encryption password.

This adds a new key to the repository and points the credentials Secret at it.
The old password is deliberately left working, so a snapshot taken before the
rotation is still readable and the change can be undone by restoring the old
Secret. Remove the old key yourself once you are sure:

    corg exec REPOSITORY -- restic key list
    corg exec REPOSITORY -- restic key remove <id>

The password is the only thing that can decrypt this repository. If the Secret
is lost and the password was not archived off-cluster, every snapshot is
unreadable forever -- which is why this refuses to run without the flag saying
you have somewhere to put it.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if !archived {
				return errors.New(
					"refusing to rotate without --i-have-somewhere-to-archive-the-new-password: " +
						"the password is the only thing that can decrypt this repository")
			}

			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}
			sb, repo, err := resolveRunTarget(cmd.Context(), c, ns, args[0])
			if err != nil {
				return err
			}
			return runRotatePassword(cmd.Context(), f, c, sb, repo)
		},
	}

	cmd.Flags().BoolVar(&archived, "i-have-somewhere-to-archive-the-new-password", false,
		"Confirm the new password will be stored off-cluster")
	return cmd
}

func newKeySecretName(repo *borgbasev1.Repository) string {
	return repo.Name + "-corg-newkey-" + rand.String(5)
}

const newKeyMountPath = "/newkey"

func runRotatePassword(
	ctx context.Context,
	f *Factory,
	c client.Client,
	sb *borgbasev1.ScheduledBackup,
	repo *borgbasev1.Repository,
) error {
	if !repo.Status.Initialized {
		return fmt.Errorf("%w: repository/%s has nothing to rotate yet", ErrNotInitialized, repo.Name)
	}

	current, err := credentials(ctx, c, repo)
	if err != nil {
		return err
	}

	password, err := secrets.GeneratePassword()
	if err != nil {
		return err
	}

	temp := &corev1.Secret{
		Namespace: repo.Namespace,
		Name:      newKeySecretName(repo),
		Labels:    map[string]string{"app.kubernetes.io/managed-by": runner.ManagedByValue},
		Data:      map[string][]byte{"password": []byte(password)},
	}
	if err := c.Create(ctx, temp); err != nil {
		return fmt.Errorf("creating the temporary key Secret: %w", err)
	}

	defer func() {
		if err := deleteTempKeySecret(c, temp); err != nil {
			p := newPrinter(f.Streams.ErrOut)
			p.printf("! could not delete the temporary key secret/%s: %v\n", temp.Name, err)
			p.printf("! it holds the new password in plaintext; delete it with: kubectl -n %s delete secret %s\n",
				temp.Namespace, temp.Name)
		}
	}()

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("adding a new key to repository/%s\n", repo.Name)
	if err := p.Err(); err != nil {
		return err
	}

	err = run.Run(ctx, sb, runner.Options{
		Purpose: "rotate",
		Command: resticCommand("key", "add",
			"--new-password-file="+newKeyMountPath+"/password"),
		ExtraVolumes: []corev1.Volume{{
			Name:   "newkey",
			Secret: &corev1.SecretVolumeSource{SecretName: temp.Name},
		}},
		ExtraMounts: []corev1.VolumeMount{
			{Name: "newkey", MountPath: newKeyMountPath, ReadOnly: true},
		},
	}, f.Streams.Out, defaultResticTimeout)
	if err != nil {
		return fmt.Errorf("adding the new key: %w", err)
	}

	patch := client.MergeFrom(current.DeepCopy())
	if current.Data == nil {
		current.Data = make(map[string][]byte, 1)
	}
	current.Data[controller.KeyResticPassword] = []byte(password)
	if err := c.Patch(ctx, current, patch); err != nil {
		return fmt.Errorf("updating %s in secret/%s: %w",
			controller.KeyResticPassword, current.Name, err)
	}

	done := newPrinter(f.Streams.Out)
	done.printf("rotated the encryption key for repository/%s\n", repo.Name)
	done.printf("  archive it now:   corg env repo/%s --show-password\n", repo.Name)
	done.printf("  the old key still works; remove it with:\n")
	done.printf("    corg exec %s -- restic key list\n", sb.Name)
	return done.Err()
}

func deleteTempKeySecret(c client.Client, temp *corev1.Secret) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
	defer cancel()

	err := c.Delete(ctx, &corev1.Secret{
		Namespace: temp.Namespace, Name: temp.Name,
	})
	if err != nil && !apierrors.IsNotFound(err) {
		return err
	}
	return nil
}

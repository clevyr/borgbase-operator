package cli

import (
	"archive/tar"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/cli/kube"
	"github.com/clevyr/borgbase-operator/internal/cli/runner"
)

// purposeRestore names the ephemeral Job a restore runs in.
const purposeRestore = "restore"

// restoreToDir streams the snapshot out of the cluster as a tar and unpacks it
// locally. Nothing is staged on disk in the cluster, so this needs no scratch
// volume and works regardless of how large the source claim is.
func restoreToDir(
	ctx context.Context, f *Factory, sb *borgbasev1.ScheduledBackup, o *restoreOptions,
) error {
	dest, err := filepath.Abs(o.toDir)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	path := o.path
	if path == "" {
		path = "/"
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("downloading %s from snapshot %s into %s\n", path, o.snapshot, dest)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Attach(ctx, sb, runner.Options{Purpose: "download"}, defaultResticTimeout,
		func(pod *corev1.Pod) error {
			reader, writer := io.Pipe()

			errCh := make(chan error, 1)
			go func() {
				// restic dump --archive tar emits a stream, so nothing is
				// written to disk on either side.
				err := run.Exec(ctx, pod, kube.ExecOptions{
					Command: resticCommand("dump", "--archive=tar", o.snapshot, path),
					Stdout:  writer,
					Stderr:  f.Streams.ErrOut,
				})
				_ = writer.CloseWithError(err)
				errCh <- err
			}()

			n, untarErr := untar(reader, dest)
			execErr := <-errCh
			if execErr != nil {
				return execErr
			}
			if untarErr != nil {
				return untarErr
			}

			done := newPrinter(f.Streams.ErrOut)
			done.printf("restored %d files into %s\n", n, dest)
			return done.Err()
		})
}

// untar unpacks a tar stream, refusing entries that would escape dest.
func untar(r io.Reader, dest string) (int, error) {
	tr := tar.NewReader(r)
	count := 0

	for {
		header, err := tr.Next()
		if err == io.EOF {
			return count, nil
		}
		if err != nil {
			return count, err
		}

		// A tar from a backup is not necessarily trusted input; an entry named
		// ../../etc/passwd must not be written outside the target directory.
		target := filepath.Join(dest, filepath.Clean("/"+header.Name))
		if !strings.HasPrefix(target, filepath.Clean(dest)+string(os.PathSeparator)) &&
			target != filepath.Clean(dest) {
			return count, fmt.Errorf("refusing entry outside the target directory: %q", header.Name)
		}

		switch header.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(header.Mode)); err != nil {
				return count, err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, err
			}
			if err := writeFile(target, os.FileMode(header.Mode), tr); err != nil {
				return count, err
			}
			count++
		case tar.TypeSymlink:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return count, err
			}
			_ = os.Remove(target)
			if err := os.Symlink(header.Linkname, target); err != nil {
				return count, err
			}
			count++
		}
	}
}

func writeFile(path string, mode os.FileMode, r io.Reader) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	if _, err := io.Copy(file, r); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}

// restoreToNewPVC stages a restore into a fresh claim, leaving the live one
// untouched so the result can be inspected before anything is committed.
func restoreToNewPVC(
	ctx context.Context, f *Factory, c client.Client, sb *borgbasev1.ScheduledBackup, o *restoreOptions,
) error {
	if sb.Spec.Volume == nil {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.volume", ErrNoSourceVolume, sb.Name)
	}

	size, err := claimSize(ctx, c, sb, o.size)
	if err != nil {
		return err
	}

	pvc := &corev1.PersistentVolumeClaim{
		Namespace: sb.Namespace,
		Name:      o.toNewPVC,
		Labels:    map[string]string{"app.kubernetes.io/managed-by": runner.ManagedByValue},
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: size},
			},
		},
	}
	if err := c.Create(ctx, pvc); err != nil {
		return fmt.Errorf("creating pvc/%s: %w", o.toNewPVC, err)
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("created pvc/%s (%s)\n", pvc.Name, size.String())
	if err := p.Err(); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	err = run.Run(ctx, sb, runner.Options{
		Purpose: purposeRestore,
		Command: resticCommand(o.resticRestoreArgs(restoreMountPath)...),
		ExtraVolumes: []corev1.Volume{{
			Name: "restore-target",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: pvc.Name,
			},
		}},
		ExtraMounts: []corev1.VolumeMount{{Name: "restore-target", MountPath: restoreMountPath}},
	}, f.Streams.Out, defaultResticTimeout)
	if err != nil {
		return err
	}

	done := newPrinter(f.Streams.ErrOut)
	done.printf("restored into pvc/%s; inspect it with:\n", pvc.Name)
	done.printf("  corg shell %s --mount-data\n", sb.Name)
	return done.Err()
}

// claimSize takes the new claim's size from the flag, else from the claim being
// restored, so the copy is guaranteed to fit.
func claimSize(
	ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup, override string,
) (resource.Quantity, error) {
	if override != "" {
		return resource.ParseQuantity(override)
	}

	var source corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.Volume.ExistingClaim}
	if err := c.Get(ctx, key, &source); err != nil {
		return resource.Quantity{}, fmt.Errorf(
			"reading pvc/%s to size the new claim: %w; pass --size to set it directly",
			sb.Spec.Volume.ExistingClaim, err)
	}

	if size, ok := source.Spec.Resources.Requests[corev1.ResourceStorage]; ok {
		return size, nil
	}
	return resource.Quantity{}, fmt.Errorf(
		"pvc/%s requests no storage size; pass --size", source.Name)
}

// restoreInPlace overwrites the live source volume.
//
// The schedule is suspended for the duration so a scheduled backup cannot run
// against a half-restored tree and capture it as the new truth.
func restoreInPlace(
	ctx context.Context, f *Factory, c client.Client, sb *borgbasev1.ScheduledBackup, o *restoreOptions,
) error {
	if sb.Spec.Volume == nil {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.volume", ErrNoSourceVolume, sb.Name)
	}

	claim := sb.Spec.Volume.ExistingClaim
	if err := confirm(f, o, fmt.Sprintf("pvc/%s in namespace %s", claim, sb.Namespace), sb.Name); err != nil {
		return err
	}

	restore, err := suspendForRestore(ctx, c, sb, o.dryRun)
	if err != nil {
		return err
	}
	// Restoring the previous suspend state is deferred so that a cancelled
	// restore cannot leave the schedule suspended.
	defer func() {
		if err := restore(); err != nil {
			p := newPrinter(f.Streams.ErrOut)
			p.printf("! could not restore spec.suspend on scheduledbackup/%s: %v\n", sb.Name, err)
			p.printf("! check it with: corg status %s\n", sb.Name)
		}
	}()

	run, err := f.Runner()
	if err != nil {
		return err
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("restoring snapshot %s over pvc/%s\n", o.snapshot, claim)
	if err := p.Err(); err != nil {
		return err
	}

	return run.Run(ctx, sb, runner.Options{
		Purpose:   purposeRestore,
		MountData: true,
		Command:   resticCommand(o.resticRestoreArgs(sb.Spec.Volume.EffectiveMountPath())...),
	}, f.Streams.Out, defaultResticTimeout)
}

// suspendForRestore suspends the schedule and returns a function that puts it
// back the way it was.
func suspendForRestore(
	ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup, dryRun bool,
) (func() error, error) {
	if dryRun || sb.Spec.Suspend {
		return func() error { return nil }, nil
	}

	patch := client.MergeFrom(sb.DeepCopy())
	sb.Spec.Suspend = true
	if err := c.Patch(ctx, sb, patch); err != nil {
		return nil, fmt.Errorf("suspending scheduledbackup/%s: %w", sb.Name, err)
	}

	return func() error {
		// Detached from the caller's context: a Ctrl-C must still un-suspend.
		ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), cleanupTimeout)
		defer cancel()

		var latest borgbasev1.ScheduledBackup
		key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}
		if err := c.Get(ctx, key, &latest); err != nil {
			return err
		}
		patch := client.MergeFrom(latest.DeepCopy())
		latest.Spec.Suspend = false
		return c.Patch(ctx, &latest, patch)
	}, nil
}

// restoreToDatabase streams the dump back through restoredb, the inverse of the
// dumpdb the backup used.
func restoreToDatabase(
	ctx context.Context, f *Factory, sb *borgbasev1.ScheduledBackup, o *restoreOptions,
) error {
	db := sb.Spec.Database
	if db == nil {
		return fmt.Errorf("%w: scheduledbackup/%s has no spec.database", ErrNoDatabase, sb.Name)
	}

	target := fmt.Sprintf("the %s database %q on %s", db.Engine, db.Name, db.Host)
	if db.Host == "" {
		target = fmt.Sprintf("the %s database", db.Engine)
	}
	if err := confirm(f, o, target, sb.Name); err != nil {
		return err
	}

	run, err := f.Runner()
	if err != nil {
		return err
	}

	// The dump is stored in the snapshot under the host name, which the backup
	// sets to the namespace, with an extension fixed by the engine.
	dumpFile := sb.Namespace + dumpExtension(db.Engine)
	pipeline := fmt.Sprintf("restic dump %s %s | restoredb %s",
		shellQuote(o.snapshot), shellQuote(dumpFile), string(db.Engine))
	if o.dryRun {
		pipeline += " --dry-run"
	}

	p := newPrinter(f.Streams.ErrOut)
	p.printf("restoring %s from snapshot %s\n", target, o.snapshot)
	if err := p.Err(); err != nil {
		return err
	}

	// Fail with something actionable rather than `restoredb: not found` from
	// the shell: the image's tags are mutable, so an older pull has no
	// restoredb even at the same tag.
	guarded := fmt.Sprintf(
		"command -v restoredb >/dev/null 2>&1 || { echo %s >&2; exit 1; }\n%s",
		shellQuote("this backup image has no restoredb; re-pull ghcr.io/clevyr/restic "+
			"or unpin spec.image"), pipeline)

	return run.Run(ctx, sb, runner.Options{
		Purpose: "restoredb",
		Command: []string{"sh", "-c", guarded},
	}, f.Streams.Out, defaultResticTimeout)
}

// dumpExtension mirrors the rule the backup image's restic wrapper uses when it
// names the dump inside a snapshot.
func dumpExtension(engine borgbasev1.DatabaseEngine) string {
	switch engine {
	case borgbasev1.DatabaseEngineCNPG:
		return ".dmp"
	default:
		return ".sql"
	}
}

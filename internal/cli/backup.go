package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

const (
	// pollInterval is how often the CLI re-reads state while waiting. The
	// operator reconciles on a watch, so a trigger is normally picked up well
	// inside the first tick.
	pollInterval = time.Second

	defaultWaitTimeout = 2 * time.Hour
)

var (
	ErrTriggerSkipped = errors.New("backup was not started")
	ErrBackupFailed   = errors.New("backup failed")
)

func newBackupCommand(f *Factory) *cobra.Command {
	var (
		waitForIt bool
		follow    bool
		timeout   time.Duration
	)

	cmd := &cobra.Command{
		Use:     "backup <name>",
		Aliases: []string{"run"},
		Short:   "Run a backup now",
		GroupID: GroupRun,
		Long: `Run a backup outside its schedule.

This annotates the ScheduledBackup and the operator does the work, so the run
is recorded in status and does exactly what a scheduled run would. The same
thing can be done with kubectl:

    kubectl annotate scheduledbackup/NAME \
      borgbase.clevyr.com/trigger-at="$(date -Is)" --overwrite

A suspended ScheduledBackup still runs: suspending the schedule and then
backing up by hand is the main reason to suspend one. A run is refused only
when one is already in flight and concurrencyPolicy is Forbid.`,
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			cs, err := f.Clientset()
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
			if target.Kind != TargetScheduledBackup {
				return fmt.Errorf("backup needs a ScheduledBackup, not a %s", target.Kind)
			}

			return runBackupNow(cmd.Context(), c, cs, f.Streams.Out,
				target.ScheduledBackup, waitForIt || follow, follow, timeout)
		},
	}

	cmd.Flags().BoolVar(&waitForIt, "wait", false, "Wait for the backup to finish")
	cmd.Flags().BoolVarP(&follow, "follow", "f", false, "Stream the backup's logs (implies --wait)")
	cmd.Flags().DurationVar(&timeout, "timeout", defaultWaitTimeout, "How long to wait")
	return cmd
}

func runBackupNow(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	sb *borgbasev1.ScheduledBackup,
	waitForIt, follow bool,
	timeout time.Duration,
) error {
	p := newPrinter(out)

	// The operator only reaches the trigger once the backup is reconcilable,
	// so say plainly that the run is deferred rather than appearing to hang.
	if cond := FindCondition(sb.Status.Conditions, borgbasev1.ScheduledBackupConditionReady); cond == nil {
		p.println("! this backup has not been reconciled yet; the run will start once it is")
	} else if cond.Status != metav1.ConditionTrue {
		p.printf("! this backup is not ready (%s); the run will start once it is\n", cond.Reason)
	}

	at := time.Now().UTC().Truncate(time.Second)
	if err := triggerBackup(ctx, c, sb, at); err != nil {
		return err
	}
	p.printf("triggered backup of scheduledbackup/%s\n", sb.Name)

	if !waitForIt {
		p.printf("  the operator will create job/%s\n", backup.ManualJobName(sb, at))
		return p.Err()
	}
	if err := p.Err(); err != nil {
		return err
	}

	jobName, err := awaitTrigger(ctx, c, sb, at, timeout)
	if err != nil {
		return err
	}
	p.printf("  started job/%s\n", jobName)
	if err := p.Err(); err != nil {
		return err
	}

	return awaitJob(ctx, c, cs, out, sb, jobName, follow, timeout)
}

// triggerBackup stamps the annotation the operator watches.
func triggerBackup(ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup, at time.Time) error {
	patch := client.MergeFrom(sb.DeepCopy())
	if sb.Annotations == nil {
		sb.Annotations = map[string]string{}
	}
	sb.Annotations[borgbasev1.AnnotationTriggerAt] = at.Format(time.RFC3339)
	return c.Patch(ctx, sb, patch)
}

// awaitTrigger waits for the operator to act on the trigger and returns the Job
// it started.
//
// It reads status rather than looking for the Job by name, so a run refused by
// concurrencyPolicy is reported as refused instead of waiting for a Job that
// will never appear.
func awaitTrigger(
	ctx context.Context,
	c client.Client,
	sb *borgbasev1.ScheduledBackup,
	at time.Time,
	timeout time.Duration,
) (string, error) {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Name}

	var jobName string
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			var latest borgbasev1.ScheduledBackup
			if err := c.Get(ctx, key, &latest); err != nil {
				return false, err
			}
			acked := latest.Status.LastTriggerTime
			if acked == nil || acked.Time.Before(at) {
				return false, nil
			}
			jobName = latest.Status.LastTriggerJob
			return true, nil
		})
	if err != nil {
		return "", fmt.Errorf("waiting for the operator to start the backup: %w", err)
	}

	if jobName == "" {
		return "", fmt.Errorf(
			"%w: a backup is already running and concurrencyPolicy is Forbid; "+
				"wait for it to finish, or use `corg status %s` to see it", ErrTriggerSkipped, sb.Name)
	}
	return jobName, nil
}

// awaitJob follows a run to completion, streaming its logs when asked.
func awaitJob(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	sb *borgbasev1.ScheduledBackup,
	jobName string,
	follow bool,
	timeout time.Duration,
) error {
	key := types.NamespacedName{Namespace: sb.Namespace, Name: jobName}

	if follow {
		if err := followJobLogs(ctx, c, cs, out, key, timeout); err != nil {
			return err
		}
	}

	var job batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, key, &job); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			return job.Status.Succeeded > 0 || job.Status.Failed > 0, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for job/%s: %w", jobName, err)
	}

	p := newPrinter(out)
	if job.Status.Failed > 0 {
		p.printf("job/%s failed\n", jobName)
		if err := p.Err(); err != nil {
			return err
		}
		return fmt.Errorf("%w: see `corg logs %s`", ErrBackupFailed, sb.Name)
	}
	p.printf("job/%s succeeded\n", jobName)
	return p.Err()
}

// followJobLogs waits for the run's pod to exist, then streams it.
func followJobLogs(
	ctx context.Context,
	c client.Client,
	cs kubernetes.Interface,
	out io.Writer,
	key types.NamespacedName,
	timeout time.Duration,
) error {
	var job batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := c.Get(ctx, key, &job); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			pods, err := podsForJob(ctx, c, &job)
			if err != nil {
				return false, err
			}
			for i := range pods {
				// A pod that is still pulling its image has nothing to stream.
				if pods[i].Status.Phase != corev1.PodPending {
					return true, nil
				}
			}
			return false, nil
		})
	if err != nil {
		return fmt.Errorf("waiting for the backup pod: %w", err)
	}

	return streamJobLogs(ctx, c, cs, out, &job, corev1.PodLogOptions{Follow: true})
}

package controller

import (
	"context"
	"fmt"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

// historyLimit bounds status.history. Enough to see a pattern of failures
// without letting the object grow without limit.
const historyLimit = 10

// reconcileTrigger runs a backup when the trigger annotation names a time newer
// than the one already acted on.
//
// A one-off run deliberately ignores spec.suspend: suspending the schedule and
// then running a backup by hand is the main reason to suspend one. It does
// respect concurrencyPolicy, because two restic processes against one
// repository would just collide on the lock.
func (r *ScheduledBackupReconciler) reconcileTrigger(
	ctx context.Context,
	sb *borgbasev1.ScheduledBackup,
	repo *borgbasev1.Repository,
	runs []observedRun,
) error {
	raw := sb.Annotations[borgbasev1.AnnotationTriggerAt]
	if raw == "" {
		return nil
	}

	at, err := time.Parse(time.RFC3339, raw)
	if err != nil {
		// A typo in an annotation should not wedge the whole reconcile.
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerInvalid", "Trigger",
			"Ignoring %s=%q: expected an RFC3339 timestamp", borgbasev1.AnnotationTriggerAt, raw)
		return nil
	}

	if sb.Status.LastTriggerTime != nil && !at.After(sb.Status.LastTriggerTime.Time) {
		return nil
	}

	wanted := backup.ManualJobName(sb, at)

	if sb.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent {
		// Creating the Job enqueues another reconcile, which can read a
		// ScheduledBackup whose status write has not reached the cache yet: the
		// trigger looks unhandled and its own new Job looks like an unrelated
		// run in progress. Recognising the name means a backup that is running
		// is never recorded as one that never started.
		if active := activeRun(runs); active != "" && active != wanted {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerSkipped", "Trigger",
				"Not starting a backup: Job %s is still running and concurrencyPolicy is Forbid", active)
			// Record the trigger anyway, so it is not retried on every pass.
			sb.Status.LastTriggerTime = &metav1.Time{Time: at}
			sb.Status.LastTriggerJob = ""
			return nil
		}
	}

	job, err := backup.BuildManualJob(sb, repo, r.Config, at)
	if err != nil {
		return err
	}
	if err := controllerutil.SetControllerReference(sb, job, r.Scheme); err != nil {
		return err
	}

	switch err := r.Create(ctx, job); {
	case err == nil:
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, "BackupTriggered", "Trigger",
			"Started backup Job %s", job.Name)
	case apierrors.IsAlreadyExists(err):
		// The name is derived from the trigger time, so this is the same
		// request arriving twice rather than a second backup.
	default:
		return fmt.Errorf("creating backup job: %w", err)
	}

	sb.Status.LastTriggerTime = &metav1.Time{Time: at}
	sb.Status.LastTriggerJob = job.Name
	return nil
}

// observedRun is a backup Job as it exists in the cluster right now.
type observedRun struct {
	run    borgbasev1.BackupRun
	active bool
}

// activeRun returns the name of a run still in flight, or "".
func activeRun(runs []observedRun) string {
	for _, o := range runs {
		if o.active {
			return o.run.JobName
		}
	}
	return ""
}

// observedRuns lists the Jobs belonging to this ScheduledBackup, newest first.
//
// Scheduled runs are owned by the generated CronJob and one-off runs by the
// ScheduledBackup itself, so both owners count.
func (r *ScheduledBackupReconciler) observedRuns(
	ctx context.Context, sb *borgbasev1.ScheduledBackup, cronJobUID types.UID,
) ([]observedRun, error) {
	var jobs batchv1.JobList
	if err := r.List(ctx, &jobs, client.InNamespace(sb.Namespace)); err != nil {
		return nil, err
	}

	out := make([]observedRun, 0, len(jobs.Items))
	for i := range jobs.Items {
		job := &jobs.Items[i]
		owner := metav1.GetControllerOf(job)
		if owner == nil || (owner.UID != sb.UID && (cronJobUID == "" || owner.UID != cronJobUID)) {
			continue
		}
		out = append(out, runFromJob(job))
	}

	slices.SortStableFunc(out, func(a, b observedRun) int {
		return compareRunTimes(b.run.StartTime, a.run.StartTime)
	})
	return out, nil
}

func runFromJob(job *batchv1.Job) observedRun {
	trigger := borgbasev1.BackupTriggerScheduled
	if job.Labels[backup.LabelTrigger] == backup.TriggerManual {
		trigger = borgbasev1.BackupTriggerManual
	}

	run := borgbasev1.BackupRun{
		JobName:        job.Name,
		Trigger:        trigger,
		StartTime:      job.Status.StartTime,
		CompletionTime: job.Status.CompletionTime,
	}
	if run.StartTime == nil {
		run.StartTime = job.CreationTimestamp.DeepCopy()
	}

	switch {
	case job.Status.Succeeded > 0:
		run.Result = borgbasev1.BackupRunSucceeded
	case job.Status.Failed > 0:
		run.Result = borgbasev1.BackupRunFailed
	default:
		return observedRun{run: withResult(run, borgbasev1.BackupRunRunning), active: true}
	}
	return observedRun{run: run}
}

func withResult(run borgbasev1.BackupRun, result borgbasev1.BackupRunResult) borgbasev1.BackupRun {
	run.Result = result
	return run
}

// mergeHistory folds the Jobs currently in the cluster into the recorded
// history, keeping entries whose Jobs have since been garbage collected. That
// is the point of recording it: Jobs are removed an hour after they finish.
func mergeHistory(existing []borgbasev1.BackupRun, observed []observedRun) []borgbasev1.BackupRun {
	merged := make([]borgbasev1.BackupRun, 0, len(existing)+len(observed))
	seen := make(map[string]int, len(existing)+len(observed))

	for _, o := range observed {
		seen[o.run.JobName] = len(merged)
		merged = append(merged, o.run)
	}
	for _, run := range existing {
		if i, ok := seen[run.JobName]; ok {
			// A run that was in progress when it was last recorded is updated
			// in place rather than duplicated.
			merged[i] = mostComplete(run, merged[i])
			continue
		}
		seen[run.JobName] = len(merged)
		merged = append(merged, run)
	}

	slices.SortStableFunc(merged, func(a, b borgbasev1.BackupRun) int {
		return compareRunTimes(b.StartTime, a.StartTime)
	})
	if len(merged) > historyLimit {
		merged = merged[:historyLimit]
	}
	return merged
}

// mostComplete prefers the live Job's view, falling back to what was recorded
// for fields the Job no longer reports.
func mostComplete(recorded, live borgbasev1.BackupRun) borgbasev1.BackupRun {
	if live.StartTime == nil {
		live.StartTime = recorded.StartTime
	}
	if live.CompletionTime == nil {
		live.CompletionTime = recorded.CompletionTime
	}
	return live
}

func compareRunTimes(a, b *metav1.Time) int {
	switch {
	case a == nil && b == nil:
		return 0
	case a == nil:
		return -1
	case b == nil:
		return 1
	}
	return a.Compare(b.Time)
}

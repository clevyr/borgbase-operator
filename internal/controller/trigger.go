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

const historyLimit = 10

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
		r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerInvalid", "Trigger",
			"Ignoring %s=%q: expected an RFC3339 timestamp", borgbasev1.AnnotationTriggerAt, raw)
		return nil
	}

	if sb.Status.LastTriggerTime != nil && !at.After(sb.Status.LastTriggerTime.Time) {
		return nil
	}

	wanted := backup.ManualJobName(sb, at)

	if sb.Spec.ConcurrencyPolicy == batchv1.ForbidConcurrent {
		if active := activeRun(runs); active != "" && active != wanted {
			r.Recorder.Eventf(sb, nil, corev1.EventTypeWarning, "TriggerSkipped", "Trigger",
				"Not starting a backup: Job %s is still running and concurrencyPolicy is Forbid", active)

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
		// The Job name is derived from the trigger timestamp, so an already-existing
		// Job is this same trigger being reconciled again. Falling through would turn
		// a retry into an error.
	default:
		return fmt.Errorf("creating backup job: %w", err)
	}

	sb.Status.LastTriggerTime = &metav1.Time{Time: at}
	sb.Status.LastTriggerJob = job.Name
	return nil
}

type observedRun struct {
	run    borgbasev1.BackupRun
	active bool
}

func activeRun(runs []observedRun) string {
	for _, o := range runs {
		if o.active {
			return o.run.JobName
		}
	}
	return ""
}

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

func mergeHistory(existing []borgbasev1.BackupRun, observed []observedRun) []borgbasev1.BackupRun {
	merged := make([]borgbasev1.BackupRun, 0, len(existing)+len(observed))
	seen := make(map[string]int, len(existing)+len(observed))

	for _, o := range observed {
		seen[o.run.JobName] = len(merged)
		merged = append(merged, o.run)
	}
	for _, run := range existing {
		if i, ok := seen[run.JobName]; ok {
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

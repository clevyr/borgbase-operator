package controller

import (
	"context"
	"fmt"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

func run(name string, minutesAgo int, result borgbasev1.BackupRunResult) borgbasev1.BackupRun {
	start := metav1.NewTime(time.Now().Add(-time.Duration(minutesAgo) * time.Minute))
	r := borgbasev1.BackupRun{
		JobName:   name,
		Trigger:   borgbasev1.BackupTriggerScheduled,
		Result:    result,
		StartTime: &start,
	}
	if result != borgbasev1.BackupRunRunning {
		done := metav1.NewTime(start.Add(4 * time.Minute))
		r.CompletionTime = &done
	}
	return r
}

func observe(runs ...borgbasev1.BackupRun) []observedRun {
	out := make([]observedRun, 0, len(runs))
	for _, r := range runs {
		out = append(out, observedRun{run: r, active: r.Result == borgbasev1.BackupRunRunning})
	}
	return out
}

func names(runs []borgbasev1.BackupRun) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.JobName)
	}
	return out
}

func TestMergeHistoryOrdersNewestFirst(t *testing.T) {
	got := mergeHistory(nil, observe(
		run("old", 300, borgbasev1.BackupRunSucceeded),
		run("new", 10, borgbasev1.BackupRunSucceeded),
		run("middle", 100, borgbasev1.BackupRunFailed),
	))

	want := []string{"new", "middle", "old"}
	if fmt.Sprint(names(got)) != fmt.Sprint(want) {
		t.Errorf("order = %v, want %v", names(got), want)
	}
}

// The whole point: Jobs are removed an hour after they finish, so a run that
// has been garbage collected must survive in history.
func TestMergeHistoryKeepsCollectedRuns(t *testing.T) {
	existing := []borgbasev1.BackupRun{
		run("gone-2", 200, borgbasev1.BackupRunSucceeded),
		run("gone-1", 300, borgbasev1.BackupRunFailed),
	}
	got := mergeHistory(existing, observe(run("still-here", 10, borgbasev1.BackupRunSucceeded)))

	want := []string{"still-here", "gone-2", "gone-1"}
	if fmt.Sprint(names(got)) != fmt.Sprint(want) {
		t.Errorf("history = %v, want %v", names(got), want)
	}
}

// An entry recorded while a run was in flight is updated, not duplicated.
func TestMergeHistoryUpdatesRunningEntryInPlace(t *testing.T) {
	existing := []borgbasev1.BackupRun{run("job-1", 10, borgbasev1.BackupRunRunning)}
	got := mergeHistory(existing, observe(run("job-1", 10, borgbasev1.BackupRunSucceeded)))

	if len(got) != 1 {
		t.Fatalf("expected 1 entry, got %d: %v", len(got), names(got))
	}
	if got[0].Result != borgbasev1.BackupRunSucceeded {
		t.Errorf("result = %q, want Succeeded", got[0].Result)
	}
	if got[0].CompletionTime == nil {
		t.Error("completionTime should be recorded once the run finishes")
	}
}

func TestMergeHistoryIsBounded(t *testing.T) {
	existing := make([]borgbasev1.BackupRun, 0, 25)
	for i := range 25 {
		existing = append(existing, run(fmt.Sprintf("job-%02d", i), i*10, borgbasev1.BackupRunSucceeded))
	}
	got := mergeHistory(existing, nil)

	if len(got) != historyLimit {
		t.Fatalf("history length = %d, want %d", len(got), historyLimit)
	}
	// Truncation must drop the oldest, not the newest.
	if got[0].JobName != "job-00" {
		t.Errorf("newest entry = %q, want job-00", got[0].JobName)
	}
}

func TestRunFromJobClassifies(t *testing.T) {
	base := func() *batchv1.Job {
		return &batchv1.Job{
			Name: "j", CreationTimestamp: metav1.Now(),
			Status: batchv1.JobStatus{StartTime: ptr.To(metav1.Now())},
		}
	}

	succeeded := base()
	succeeded.Status.Succeeded = 1
	if got := runFromJob(succeeded); got.run.Result != borgbasev1.BackupRunSucceeded || got.active {
		t.Errorf("succeeded job = %+v", got)
	}

	failed := base()
	failed.Status.Failed = 1
	if got := runFromJob(failed); got.run.Result != borgbasev1.BackupRunFailed || got.active {
		t.Errorf("failed job = %+v", got)
	}

	running := base()
	running.Status.Active = 1
	if got := runFromJob(running); got.run.Result != borgbasev1.BackupRunRunning || !got.active {
		t.Errorf("running job = %+v", got)
	}

	manual := base()
	manual.Labels = map[string]string{backup.LabelTrigger: backup.TriggerManual}
	manual.Status.Succeeded = 1
	if got := runFromJob(manual); got.run.Trigger != borgbasev1.BackupTriggerManual {
		t.Errorf("trigger = %q, want Manual", got.run.Trigger)
	}
	if got := runFromJob(succeeded); got.run.Trigger != borgbasev1.BackupTriggerScheduled {
		t.Errorf("unlabelled job should read as Scheduled, got %q", got.run.Trigger)
	}
}

// A Job that has not been scheduled yet has no startTime.
func TestRunFromJobFallsBackToCreationTime(t *testing.T) {
	created := metav1.NewTime(time.Now().Add(-time.Minute))
	job := &batchv1.Job{Name: "j", CreationTimestamp: created}

	got := runFromJob(job)
	if got.run.StartTime == nil || !got.run.StartTime.Time.Equal(created.Time) {
		t.Errorf("startTime = %v, want the creation timestamp %v", got.run.StartTime, created)
	}
}

// End to end: a triggered run shows up in status.history.
func TestHistoryRecordsATriggeredRun(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), triggeredBackup(triggerAt, nil))
	reconcileBackup(t, r)

	// The Job exists now; a second pass folds it into history.
	jobs := manualJobs(t, c)
	if len(jobs) != 1 {
		t.Fatalf("expected 1 manual Job, got %d", len(jobs))
	}
	jobs[0].Status.Succeeded = 1
	jobs[0].Status.StartTime = ptr.To(metav1.Now())
	jobs[0].Status.CompletionTime = ptr.To(metav1.Now())
	if err := c.Status().Update(context.Background(), &jobs[0]); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	history := loadBackup(t, c).Status.History
	if len(history) != 1 {
		t.Fatalf("expected 1 history entry, got %d", len(history))
	}
	if history[0].JobName != jobs[0].Name {
		t.Errorf("jobName = %q, want %q", history[0].JobName, jobs[0].Name)
	}
	if history[0].Trigger != borgbasev1.BackupTriggerManual {
		t.Errorf("trigger = %q, want Manual", history[0].Trigger)
	}
	if history[0].Result != borgbasev1.BackupRunSucceeded {
		t.Errorf("result = %q, want Succeeded", history[0].Result)
	}
}

// Unrelated Jobs in the same namespace must not appear in history.
func TestHistoryIgnoresForeignJobs(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	stranger := &batchv1.Job{
		Namespace: testNS, Name: "someone-elses",
		OwnerReferences: []metav1.OwnerReference{{
			APIVersion: "batch/v1", Kind: "CronJob",
			Name: "other", UID: "uid-stranger", Controller: ptr.To(true),
		}},
		Status: batchv1.JobStatus{Succeeded: 1},
	}
	if err := c.Create(context.Background(), stranger); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if history := loadBackup(t, c).Status.History; len(history) != 0 {
		t.Errorf("expected no history, got %v", names(history))
	}
}

package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	fakeclientset "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func trigger(t *testing.T, c client.Client, sb *borgbasev1.ScheduledBackup, wait bool) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runBackupNow(context.Background(), c, fakeclientset.NewSimpleClientset(), &buf,
		sb, wait, false, 2*time.Second)
	return buf.String(), err
}

func TestBackupStampsTheTriggerAnnotation(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, sb)

	out, err := trigger(t, c, sb, false)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}

	var got borgbasev1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: testBackupName}, &got); err != nil {
		t.Fatal(err)
	}

	raw := got.Annotations[borgbasev1.AnnotationTriggerAt]
	if raw == "" {
		t.Fatal("the trigger annotation was not set")
	}
	// The operator parses this as RFC3339; anything else is silently ignored.
	if _, err := time.Parse(time.RFC3339, raw); err != nil {
		t.Errorf("annotation %q is not RFC3339: %v", raw, err)
	}
	for _, want := range []string{"triggered backup", "will create job/"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// The operator never reaches the trigger for a backup it cannot reconcile, so
// the CLI must say the run is deferred rather than appear to hang.
func TestBackupWarnsWhenNotReady(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	sb.Status.Conditions = []metav1.Condition{{
		Type: borgbasev1.ScheduledBackupConditionReady, Status: metav1.ConditionFalse,
		Reason: "RepositoryNotReady", Message: "not initialized",
	}}
	c := newClient(t, sb)

	out, err := trigger(t, c, sb, false)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}
	if !strings.Contains(out, "RepositoryNotReady") || !strings.Contains(out, "once it is") {
		t.Errorf("expected a deferred-run warning:\n%s", out)
	}
}

// A run refused by concurrencyPolicy must be reported, not waited on. The
// operator acknowledges the trigger without a Job, which is what distinguishes
// "refused" from "not picked up yet".
func TestBackupReportsASkippedTrigger(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	acked := metav1.NewTime(time.Now().Add(time.Hour))
	sb.Status.LastTriggerTime = &acked
	sb.Status.LastTriggerJob = ""
	c := newClient(t, sb)

	_, err := trigger(t, c, sb, true)
	if !errors.Is(err, ErrTriggerSkipped) {
		t.Fatalf("expected ErrTriggerSkipped, got %v", err)
	}
	if !strings.Contains(err.Error(), "Forbid") {
		t.Errorf("the error should explain why: %v", err)
	}
}

// The happy path: the operator names the Job it started and the CLI follows it.
func TestBackupWaitsForAnAcknowledgedRun(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	acked := metav1.NewTime(time.Now().Add(time.Hour))
	sb.Status.LastTriggerTime = &acked
	sb.Status.LastTriggerJob = testManualJob

	done := jobOwnedBy(testManualJob, sb.UID, "ScheduledBackup", time.Minute)
	done.Status.Succeeded = 1
	c := newClient(t, sb, done)

	out, err := trigger(t, c, sb, true)
	if err != nil {
		t.Fatalf("runBackupNow: %v", err)
	}
	for _, want := range []string{"started job/web-files-manual-abc", "succeeded"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// A failed run must exit non-zero so it can gate a script.
func TestBackupFailsOnAFailedRun(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	acked := metav1.NewTime(time.Now().Add(time.Hour))
	sb.Status.LastTriggerTime = &acked
	sb.Status.LastTriggerJob = testManualJob

	failed := jobOwnedBy(testManualJob, sb.UID, "ScheduledBackup", time.Minute)
	failed.Status.Failed = 1
	c := newClient(t, sb, failed)

	if _, err := trigger(t, c, sb, true); !errors.Is(err, ErrBackupFailed) {
		t.Fatalf("expected ErrBackupFailed, got %v", err)
	}
}

func TestCancelDeletesRunningJobsOnly(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID

	running := jobOwnedBy("running", cj.UID, "CronJob", time.Minute)
	running.Status.Active = 1
	finished := jobOwnedBy("finished", cj.UID, "CronJob", time.Hour)
	finished.Status.Succeeded = 1

	c := newClient(t, sb, cj, running, finished)

	var buf bytes.Buffer
	if err := runCancel(context.Background(), c, &buf, sb); err != nil {
		t.Fatalf("runCancel: %v", err)
	}
	if !strings.Contains(buf.String(), "cancelled job/running") {
		t.Errorf("expected the running job to be cancelled:\n%s", buf.String())
	}
	// The lock warning matters: a killed restic leaves one behind.
	if !strings.Contains(buf.String(), "unlock") {
		t.Errorf("expected a stale-lock warning:\n%s", buf.String())
	}

	var jobs batchv1.JobList
	if err := c.List(context.Background(), &jobs, client.InNamespace(testNS)); err != nil {
		t.Fatal(err)
	}
	if len(jobs.Items) != 1 || jobs.Items[0].Name != "finished" {
		t.Errorf("a completed job must be left alone, got %+v", jobs.Items)
	}
}

func TestCancelWithNothingRunning(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	var buf bytes.Buffer
	if err := runCancel(context.Background(), newClient(t, sb), &buf, sb); err != nil {
		t.Fatalf("runCancel: %v", err)
	}
	if !strings.Contains(buf.String(), "no backup is running") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestWaitReturnsWhenNothingIsRunning(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	var buf bytes.Buffer
	if err := runWait(context.Background(), newClient(t, sb), &buf, sb, time.Second); err != nil {
		t.Fatalf("runWait: %v", err)
	}
	if !strings.Contains(buf.String(), "no backup is running") {
		t.Errorf("unexpected output: %q", buf.String())
	}
}

func TestWaitFailsOnAFailedRun(t *testing.T) {
	sb := readyBackup(testBackupName, testRepoName)
	cj := ownedCronJob(sb)
	cj.UID = testCronJobUID
	running := jobOwnedBy("running", cj.UID, "CronJob", time.Minute)
	running.Status.Active = 1
	c := newClient(t, sb, cj, running)

	go func() {
		time.Sleep(50 * time.Millisecond)
		var job batchv1.Job
		key := types.NamespacedName{Namespace: testNS, Name: "running"}
		if err := c.Get(context.Background(), key, &job); err != nil {
			return
		}
		job.Status.Active, job.Status.Failed = 0, 1
		_ = c.Status().Update(context.Background(), &job)
	}()

	var buf bytes.Buffer
	err := runWait(context.Background(), c, &buf, sb, 5*time.Second)
	if !errors.Is(err, ErrBackupFailed) {
		t.Fatalf("expected ErrBackupFailed, got %v", err)
	}
}

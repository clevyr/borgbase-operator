package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func status(t *testing.T, c client.Client, arg string) string {
	t.Helper()
	var buf bytes.Buffer
	if err := runStatus(context.Background(), c, &buf, "prod", arg, defaultRunLimit); err != nil {
		t.Fatalf("runStatus: %v", err)
	}
	return buf.String()
}

func TestStatusBackup(t *testing.T) {
	r := readyRepo("prod", "store")
	sb := readyBackup("prod", "web-files", "store")
	cj := ownedCronJob(sb)
	cj.UID = "uid-cronjob"

	good := jobOwnedBy("web-files-backup-2", cj.UID, "CronJob", 2*time.Hour)
	done := metav1.NewTime(good.Status.StartTime.Add(4*time.Minute + 12*time.Second))
	good.Status.CompletionTime = &done
	good.Status.Succeeded = 1

	bad := jobOwnedBy("web-files-backup-1", cj.UID, "CronJob", 26*time.Hour)
	badDone := metav1.NewTime(bad.Status.StartTime.Add(18 * time.Second))
	bad.Status.CompletionTime = &badDone
	bad.Status.Failed = 1

	out := status(t, newClient(t, r, sb, cj, good, bad), "sb/web-files")

	for _, want := range []string{
		"scheduledbackup/web-files",
		"Repository", "store", "2.1 TiB / 4 TiB",
		"Schedule", "17 2 * * *", "America/Chicago",
		"CONDITION", "Ready", "True",
		"RESULT", "STARTED", "DURATION", "JOB",
		"succeeded", "4m12s", "web-files-backup-2",
		"failed", "18s", "web-files-backup-1",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
	// Newest run first.
	if strings.Index(out, "web-files-backup-2") > strings.Index(out, "web-files-backup-1") {
		t.Errorf("runs are not newest-first:\n%s", out)
	}
}

func TestStatusRepository(t *testing.T) {
	out := status(t, newClient(t, readyRepo("prod", "store")), "repo/store")
	for _, want := range []string{
		"repository/store", "BorgBase ID", "abcd1234",
		"abcd1234.repo.borgbase.com", "2.1 TiB / 4 TiB", "Initialized", "True",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

func TestStatusNoRuns(t *testing.T) {
	r := readyRepo("prod", "store")
	sb := readyBackup("prod", "web-files", "store")
	out := status(t, newClient(t, r, sb), "sb/web-files")
	if !strings.Contains(out, "No runs are still present") {
		t.Errorf("expected the empty-runs notice:\n%s", out)
	}
}

// A dangling repositoryRef must not stop status from rendering.
func TestStatusToleratesMissingRepository(t *testing.T) {
	sb := readyBackup("prod", "orphan", "gone")
	out := status(t, newClient(t, sb), "sb/orphan")
	if !strings.Contains(out, "unreadable") {
		t.Errorf("expected the repository error inline:\n%s", out)
	}
	if !strings.Contains(out, "Schedule") {
		t.Errorf("the rest of the status should still render:\n%s", out)
	}
}

func TestRunResultAndDuration(t *testing.T) {
	running := jobOwnedBy("j", "u", "CronJob", time.Minute)
	running.Status.Active = 1
	if got := runResult(running); got != "running" {
		t.Errorf("runResult(active) = %q", got)
	}

	pending := jobOwnedBy("j", "u", "CronJob", time.Minute)
	pending.Status.StartTime = nil
	if got := runDuration(pending); got != "-" {
		t.Errorf("runDuration(no start) = %q, want -", got)
	}

	// A failed Job with no completion time falls back to its failure condition
	// rather than measuring up to now.
	failed := jobOwnedBy("j", "u", "CronJob", 10*time.Hour)
	failed.Status.Failed = 1
	failed.Status.Conditions = []batchv1.JobCondition{{
		Type:               batchv1.JobFailed,
		LastTransitionTime: metav1.NewTime(failed.Status.StartTime.Add(30 * time.Second)),
	}}
	if got := runDuration(failed); got != "30s" {
		t.Errorf("runDuration(failed) = %q, want 30s", got)
	}
}

func TestUsageOf(t *testing.T) {
	r := newRepo("prod", "store")
	if got := usageOf(r); got != "<unknown>" {
		t.Errorf("usageOf(empty) = %q", got)
	}
	r.Status.CurrentUsage = "2.1 TiB"
	if got := usageOf(r); got != "2.1 TiB" {
		t.Errorf("usageOf(no quota) = %q", got)
	}
	r.Status.Quota = "4 TiB"
	if got := usageOf(r); got != "2.1 TiB / 4 TiB" {
		t.Errorf("usageOf(both) = %q", got)
	}
}

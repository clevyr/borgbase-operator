package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"github.com/clevyr/borgbase-operator/internal/healthchecks"
)

const (
	cronJobName = resticName + "-backup"
	testRepoID  = "abc12345"
)

func backupHarness(t *testing.T, objs ...client.Object) (*ScheduledBackupReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&borgbasev1.ScheduledBackup{}, &borgbasev1.Repository{}).
		Build()
	return &ScheduledBackupReconciler{
		Client:   c,
		Scheme:   scheme,
		Recorder: events.NewFakeRecorder(100),
		Config: backup.Config{
			Image:        "ghcr.io/clevyr/restic:test",
			Healthchecks: healthchecks.Config{Enabled: false},
		},
	}, c
}

func initializedRepo() *borgbasev1.Repository {
	return &borgbasev1.Repository{
		Name: resticName, Namespace: testNS,
		Status: borgbasev1.RepositoryStatus{Initialized: true, RepositoryID: testRepoID},
	}
}

func scheduledBackup() *borgbasev1.ScheduledBackup {
	return &borgbasev1.ScheduledBackup{
		Name: resticName, Namespace: testNS,
		Spec: borgbasev1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: resticName},
			Schedule:      "@hourly",
			Sources:       []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
		},
	}
}

// A CronJob the operator owns is updated in place, so drift is corrected.
func TestUpdatesItsOwnCronJob(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("first reconcile: %v", err)
	}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatalf("expected a CronJob: %v", err)
	}

	// Simulate a manual edit, which the next reconcile should correct.
	want := cj.Spec.Schedule
	cj.Spec.Schedule = "0 0 * * *"
	if err := c.Update(context.Background(), &cj); err != nil {
		t.Fatal(err)
	}
	if _, err := r.Reconcile(context.Background(), ctrl.Request{NamespacedName: key}); err != nil {
		t.Fatalf("second reconcile: %v", err)
	}
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	if cj.Spec.Schedule != want {
		t.Errorf("drift was not corrected: schedule = %q, want %q", cj.Spec.Schedule, want)
	}
}

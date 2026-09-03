package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
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

	newBackupName = "restic-new"
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

func TestRepositoryNotFoundSetsCondition(t *testing.T) {
	r, c := backupHarness(t, scheduledBackup())
	reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Reason != "RepositoryNotFound" {
		t.Errorf("Ready condition = %+v, want reason RepositoryNotFound", cond)
	}
	assertNoCronJob(t, c)
}

func TestRepositoryNotInitializedSetsCondition(t *testing.T) {
	repo := initializedRepo()
	repo.Status.Initialized = false
	r, c := backupHarness(t, repo, scheduledBackup())
	reconcileBackup(t, r)

	if cond := readyCondition(t, c); cond == nil || cond.Reason != "RepositoryNotReady" {
		t.Errorf("Ready condition = %+v, want reason RepositoryNotReady", cond)
	}
	assertNoCronJob(t, c)
}

func TestCreatesCacheVolume(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	if err := c.Get(context.Background(), key, &pvc); err != nil {
		t.Fatalf("expected a cache volume: %v", err)
	}
	if owner := metav1.GetControllerOf(&pvc); owner == nil || owner.Name != resticName {
		t.Errorf("cache volume owner = %+v, want the ScheduledBackup", owner)
	}
}

func TestDisablingCacheDeletesTheVolume(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	var pvc corev1.PersistentVolumeClaim
	if err := c.Get(context.Background(), key, &pvc); err != nil {
		t.Fatalf("expected a cache volume: %v", err)
	}

	var sb borgbasev1.ScheduledBackup
	sbKey := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), sbKey, &sb); err != nil {
		t.Fatal(err)
	}
	sb.Spec.Cache = &borgbasev1.CacheSpec{Enabled: ptr.To(false)}
	if err := c.Update(context.Background(), &sb); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	if err := c.Get(context.Background(), key, &pvc); !apierrors.IsNotFound(err) {
		t.Errorf("cache volume survived being disabled: %v", err)
	}
}

func TestDisablingCacheLeavesForeignClaimAlone(t *testing.T) {
	foreign := &corev1.PersistentVolumeClaim{
		Name: resticName + "-cache", Namespace: testNS,
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("1Gi")},
			},
		},
	}
	sb := scheduledBackup()
	sb.Spec.Cache = &borgbasev1.CacheSpec{Enabled: ptr.To(false)}

	r, c := backupHarness(t, initializedRepo(), sb, foreign)
	reconcileBackup(t, r)

	key := types.NamespacedName{Namespace: testNS, Name: resticName + "-cache"}
	if err := c.Get(context.Background(), key, &corev1.PersistentVolumeClaim{}); err != nil {
		t.Errorf("an unowned claim was deleted: %v", err)
	}
}

func TestCopiesCronJobStatusTimes(t *testing.T) {
	r, c := backupHarness(t, initializedRepo(), scheduledBackup())
	reconcileBackup(t, r)

	cjKey := types.NamespacedName{Namespace: testNS, Name: cronJobName}
	var cj batchv1.CronJob
	if err := c.Get(context.Background(), cjKey, &cj); err != nil {
		t.Fatal(err)
	}
	scheduled := metav1.NewTime(time.Now().Add(-time.Hour).Truncate(time.Second))
	succeeded := metav1.NewTime(time.Now().Add(-30 * time.Minute).Truncate(time.Second))
	cj.Status.LastScheduleTime = &scheduled
	cj.Status.LastSuccessfulTime = &succeeded
	cj.Status.Active = []corev1.ObjectReference{{Name: "run-1"}}
	if err := c.Status().Update(context.Background(), &cj); err != nil {
		t.Fatal(err)
	}
	reconcileBackup(t, r)

	var sb borgbasev1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	if sb.Status.LastSuccessfulTime == nil || !sb.Status.LastSuccessfulTime.Equal(&succeeded) {
		t.Errorf("lastSuccessfulTime = %v, want %v", sb.Status.LastSuccessfulTime, succeeded)
	}
	if sb.Status.Active != 1 {
		t.Errorf("active = %d, want 1", sb.Status.Active)
	}
}

func TestSlugConflictBlocksTheNewerBackup(t *testing.T) {
	pingKey := &corev1.SecretKeySelector{
		Name: "healthchecks-ping-key", Key: "PING_KEY",
	}
	incumbent := scheduledBackup()
	incumbent.Name = "restic-old"
	incumbent.CreationTimestamp = metav1.NewTime(time.Now().Add(-time.Hour))
	incumbent.Spec.Healthchecks = &borgbasev1.HealthchecksSpec{PingKeySecretRef: pingKey}

	newcomer := scheduledBackup()
	newcomer.Name = newBackupName
	newcomer.CreationTimestamp = metav1.NewTime(time.Now())
	newcomer.Spec.Healthchecks = &borgbasev1.HealthchecksSpec{PingKeySecretRef: pingKey}

	r, c := backupHarness(t, initializedRepo(), incumbent, newcomer)
	r.Config.Healthchecks = healthchecks.Config{
		Enabled: true, APIURL: "http://healthchecks:8000/ping", AutoCreate: true,
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: newBackupName,
	}); err != nil {
		t.Fatalf("reconciling the newcomer: %v", err)
	}
	var got borgbasev1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: newBackupName}, &got); err != nil {
		t.Fatal(err)
	}
	cond := apimeta.FindStatusCondition(got.Status.Conditions, borgbasev1.ScheduledBackupConditionReady)
	if cond == nil || cond.Reason != "SlugConflict" {
		t.Fatalf("newcomer Ready = %+v, want reason SlugConflict", cond)
	}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: newBackupName + "-backup"},
		&batchv1.CronJob{}); !apierrors.IsNotFound(err) {
		t.Error("the conflicting backup was scheduled anyway")
	}

	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: "restic-old",
	}); err != nil {
		t.Fatalf("reconciling the incumbent: %v", err)
	}
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: "restic-old-backup"},
		&batchv1.CronJob{}); err != nil {
		t.Errorf("the older backup should still be scheduled: %v", err)
	}
}

func TestDistinctSlugsCoexist(t *testing.T) {
	pingKey := &corev1.SecretKeySelector{Name: "healthchecks-ping-key", Key: "PING_KEY"}
	first := scheduledBackup()
	first.Name = "restic-files"
	first.Spec.Healthchecks = &borgbasev1.HealthchecksSpec{
		PingKeySecretRef: pingKey, Slug: "myapp-files",
	}
	second := scheduledBackup()
	second.Name = "restic-db"
	second.Spec.Healthchecks = &borgbasev1.HealthchecksSpec{
		PingKeySecretRef: pingKey, Slug: "myapp-db",
	}

	r, c := backupHarness(t, initializedRepo(), first, second)
	r.Config.Healthchecks = healthchecks.Config{
		Enabled: true, APIURL: "http://healthchecks:8000/ping", AutoCreate: true,
	}

	for _, name := range []string{"restic-files", "restic-db"} {
		if _, err := r.Reconcile(context.Background(), ctrl.Request{
			Namespace: testNS, Name: name,
		}); err != nil {
			t.Fatalf("reconciling %s: %v", name, err)
		}
		if err := c.Get(context.Background(),
			types.NamespacedName{Namespace: testNS, Name: name + "-backup"},
			&batchv1.CronJob{}); err != nil {
			t.Errorf("%s was not scheduled: %v", name, err)
		}
	}
}

func TestBackupsForRepositoryUsesTheIndex(t *testing.T) {
	mine := scheduledBackup()
	other := scheduledBackup()
	other.Name = "unrelated"
	other.Spec.RepositoryRef = corev1.LocalObjectReference{Name: "different-repo"}

	scheme := testScheme(t)
	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(mine, other).
		WithIndex(&borgbasev1.ScheduledBackup{}, repositoryRefField,
			func(o client.Object) []string {
				sb, ok := o.(*borgbasev1.ScheduledBackup)
				if !ok || sb.Spec.RepositoryRef.Name == "" {
					return nil
				}
				return []string{sb.Spec.RepositoryRef.Name}
			}).
		Build()
	r := &ScheduledBackupReconciler{Client: c, Scheme: scheme}

	got := r.backupsForRepository(context.Background(), initializedRepo())
	if len(got) != 1 || got[0].Name != resticName {
		t.Errorf("backupsForRepository() = %v, want just %s", got, resticName)
	}
}

func TestUnmanagedCronJobIsReported(t *testing.T) {
	foreign := &batchv1.CronJob{
		Name: cronJobName, Namespace: testNS,
		Spec: batchv1.CronJobSpec{
			Schedule: "0 0 * * *",
			JobTemplate: batchv1.JobTemplateSpec{
				Spec: batchv1.JobSpec{
					Template: corev1.PodTemplateSpec{
						Spec: corev1.PodSpec{
							RestartPolicy: corev1.RestartPolicyNever,
							Containers:    []corev1.Container{{Name: "c", Image: "busybox"}},
						},
					},
				},
			},
		},
	}
	r, _ := backupHarness(t, initializedRepo(), scheduledBackup())

	if err := r.Create(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	r.Client = &createConflictClient{Client: r.Client}
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	})
	if err == nil || !strings.Contains(err.Error(), "not managed by this operator") {
		t.Errorf("error = %v, want it to name the unmanaged CronJob", err)
	}
}

type createConflictClient struct {
	client.Client
}

func (c *createConflictClient) Get(
	ctx context.Context, key client.ObjectKey, obj client.Object, opts ...client.GetOption,
) error {
	if _, ok := obj.(*batchv1.CronJob); ok {
		return apierrors.NewNotFound(batchv1.Resource("cronjobs"), key.Name)
	}
	return c.Client.Get(ctx, key, obj, opts...)
}

func reconcileBackup(t *testing.T, r *ScheduledBackupReconciler) {
	t.Helper()
	if _, err := r.Reconcile(context.Background(), ctrl.Request{
		Namespace: testNS, Name: resticName,
	}); err != nil {
		t.Fatalf("reconcile: %v", err)
	}
}

func readyCondition(t *testing.T, c client.Client) *metav1.Condition {
	t.Helper()
	var sb borgbasev1.ScheduledBackup
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &sb); err != nil {
		t.Fatal(err)
	}
	return apimeta.FindStatusCondition(sb.Status.Conditions, borgbasev1.ScheduledBackupConditionReady)
}

func assertNoCronJob(t *testing.T, c client.Client) {
	t.Helper()
	err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: cronJobName}, &batchv1.CronJob{})
	if !apierrors.IsNotFound(err) {
		t.Errorf("a CronJob was created despite the repository not being usable: %v", err)
	}
}

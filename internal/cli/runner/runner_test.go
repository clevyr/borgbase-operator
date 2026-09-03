package runner

import (
	"context"
	"errors"
	"strings"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

const (
	testNS     = "prod"
	testName   = "web-files"
	testImage  = "ghcr.io/clevyr/restic:test"
	testRepo   = "store"
	testDBName = "app"
)

var testCommand = []string{"restic", "snapshots"}

func fixture(t *testing.T, mutate func(*borgbasev1.ScheduledBackup)) (*Runner, *borgbasev1.ScheduledBackup) {
	t.Helper()

	sb := &borgbasev1.ScheduledBackup{
		Namespace: testNS, Name: testName, UID: "uid-sb",
		Spec: borgbasev1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: testRepo},
			Schedule:      "@hourly",
			Sources:       []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
		},
	}
	if mutate != nil {
		mutate(sb)
	}
	repo := &borgbasev1.Repository{Namespace: testNS, Name: testRepo}

	cj, err := backup.BuildCronJob(sb, repo, backup.Config{Image: testImage})
	if err != nil {
		t.Fatalf("BuildCronJob: %v", err)
	}

	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		borgbasev1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	objs := []client.Object{sb, repo, cj}
	c := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
	return &Runner{Client: c}, sb
}

func containerOf(t *testing.T, job *batchv1.Job) *corev1.Container {
	t.Helper()
	c := findContainer(&job.Spec.Template.Spec)
	if c == nil {
		t.Fatal("no restic container in the built job")
	}
	return c
}

func hasVolume(job *batchv1.Job, name string) bool {
	for _, v := range job.Spec.Template.Spec.Volumes {
		if v.Name == name {
			return true
		}
	}
	return false
}

func TestBuildInheritsTheOperatorsConfiguration(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	c := containerOf(t, job)
	if c.Image != testImage {
		t.Errorf("image = %q, want the CronJob's %q", c.Image, testImage)
	}
	if strings.Join(c.Command, " ") != strings.Join(testCommand, " ") {
		t.Errorf("command = %v, want the override", c.Command)
	}

	if len(c.EnvFrom) == 0 || c.EnvFrom[0].SecretRef == nil {
		t.Errorf("envFrom was not inherited: %+v", c.EnvFrom)
	}

	if envOf(c, "RESTIC_HOST") == nil {
		t.Error("RESTIC_HOST was not inherited")
	}
	if job.Spec.BackoffLimit == nil || *job.Spec.BackoffLimit != 0 {
		t.Errorf("backoffLimit = %v, want 0", job.Spec.BackoffLimit)
	}
}

func TestBuildIsNotLabelledAsOperatorManaged(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	for _, labels := range []map[string]string{job.Labels, job.Spec.Template.Labels} {
		if got := labels[labelManagedBy]; got != ManagedByValue {
			t.Errorf("managed-by = %q, want %q", got, ManagedByValue)
		}
	}
}

func TestBuildUsesScratchCacheByDefault(t *testing.T) {
	r, sb := fixture(t, nil)
	ctx := context.Background()

	job, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	cache := volumeOf(job, cacheVolume)
	if cache == nil {
		t.Fatal("the cache volume was dropped; RESTIC_CACHE_DIR would break")
	}
	if cache.EmptyDir == nil || cache.PersistentVolumeClaim != nil {
		t.Errorf("cache volume = %+v, want an emptyDir", cache)
	}

	shared, err := r.Build(ctx, sb, Options{Command: testCommand, MountCache: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if volumeOf(shared, cacheVolume).PersistentVolumeClaim == nil {
		t.Error("--mount-cache should use the real claim")
	}
}

func TestBuildDataVolumeAndAffinity(t *testing.T) {
	withVolume := func(sb *borgbasev1.ScheduledBackup) {
		sb.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "app-data", ReadOnly: true}
		sb.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeFiles}}
	}
	r, sb := fixture(t, withVolume)
	ctx := context.Background()

	bare, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasVolume(bare, dataVolume) {
		t.Error("the data volume should be dropped unless requested")
	}
	if aff := bare.Spec.Template.Spec.Affinity; aff != nil && aff.PodAffinity != nil &&
		aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution != nil {
		t.Error("required pod affinity should be dropped with the data volume")
	}

	restore, err := r.Build(ctx, sb, Options{Command: testCommand, MountData: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	data := volumeOf(restore, dataVolume)
	if data == nil || data.PersistentVolumeClaim == nil {
		t.Fatal("--mount-data should attach the source claim")
	}
	if data.PersistentVolumeClaim.ReadOnly {
		t.Error("the source claim must be writable for a restore")
	}
	for _, m := range containerOf(t, restore).VolumeMounts {
		if m.Name == dataVolume && m.ReadOnly {
			t.Error("the data mount must be writable for a restore")
		}
	}
	aff := restore.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil ||
		aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution == nil {
		t.Error("the hard affinity must be kept when the claim is mounted")
	}
}

func TestBuildKeepsMariaDBClientLabel(t *testing.T) {
	r, sb := fixture(t, func(sb *borgbasev1.ScheduledBackup) {
		sb.Spec.Database = &borgbasev1.DatabaseSpec{
			Engine: borgbasev1.DatabaseEngineMariaDB,
			Host:   "mariadb", Name: "app", User: "root",
		}
		sb.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeMariaDB}}
	})

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if got := job.Spec.Template.Labels["mariadb-client"]; got != "true" {
		t.Errorf("mariadb-client = %q, want true", got)
	}
}

func TestBuildInteractiveIdles(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{TTY: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	c := containerOf(t, job)
	if strings.Join(c.Command, " ") != strings.Join(idleCommand, " ") {
		t.Errorf("command = %v, want the idle command so exec can attach", c.Command)
	}
	if !c.Stdin || !c.TTY {
		t.Error("an interactive pod needs stdin and tty")
	}
}

func TestBuildWithoutACronJob(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := borgbasev1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := batchv1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	sb := &borgbasev1.ScheduledBackup{Namespace: testNS, Name: testName}
	r := &Runner{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(sb).Build()}

	_, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if !errors.Is(err, ErrNoCronJob) {
		t.Fatalf("expected ErrNoCronJob, got %v", err)
	}
	if !strings.Contains(err.Error(), "corg doctor") {
		t.Errorf("the error should point somewhere useful: %v", err)
	}
}

func TestJobNameFitsAndIsUnique(t *testing.T) {
	long := &borgbasev1.ScheduledBackup{Name: strings.Repeat("a", 80)}
	name := jobName(long, "snapshots")
	if len(name) > 52 {
		t.Errorf("job name is %d chars, want <= 52: %q", len(name), name)
	}
	if jobName(long, "snapshots") == name {
		t.Error("job names must not collide between runs")
	}
}

func TestBlockedReason(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ImagePullBackOff", Message: "manifest unknown",
			}},
		}},
	}}
	if got := blockedReason(pod); !strings.Contains(got, "ImagePullBackOff") {
		t.Errorf("blockedReason = %q", got)
	}

	starting := &corev1.Pod{Status: corev1.PodStatus{
		ContainerStatuses: []corev1.ContainerStatus{{
			State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{
				Reason: "ContainerCreating",
			}},
		}},
	}}
	if got := blockedReason(starting); got != "" {
		t.Errorf("a pod that is merely starting must not be reported: %q", got)
	}
}

func volumeOf(job *batchv1.Job, name string) *corev1.Volume {
	for i := range job.Spec.Template.Spec.Volumes {
		if job.Spec.Template.Spec.Volumes[i].Name == name {
			return &job.Spec.Template.Spec.Volumes[i]
		}
	}
	return nil
}

func envOf(c *corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func TestPreflightCatchesAMissingSecret(t *testing.T) {
	r, sb := fixture(t, func(sb *borgbasev1.ScheduledBackup) {
		sb.Spec.Database = &borgbasev1.DatabaseSpec{
			Engine: borgbasev1.DatabaseEngineCNPG, Host: "postgresql-rw", Name: testDBName, User: testDBName,
		}
	})

	if err := r.Client.Create(context.Background(),
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-borgbase"}); err != nil {
		t.Fatal(err)
	}

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand, MountDatabase: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	err = r.Preflight(context.Background(), job)
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency, got %v", err)
	}

	if !strings.Contains(err.Error(), "postgresql-app") {
		t.Errorf("error should name the missing Secret: %v", err)
	}
}

func TestPreflightChecksEnvFromSecrets(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}

	err = r.Preflight(context.Background(), job)
	if !errors.Is(err, ErrMissingDependency) {
		t.Fatalf("expected ErrMissingDependency for the credentials Secret, got %v", err)
	}
}

func TestPreflightPassesWhenDependenciesExist(t *testing.T) {
	r, sb := fixture(t, nil)

	job, err := r.Build(context.Background(), sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if err := r.Client.Create(context.Background(),
		&corev1.Secret{Namespace: testNS, Name: testRepo + "-borgbase"}); err != nil {
		t.Fatal(err)
	}
	if err := r.Client.Create(context.Background(),
		&corev1.PersistentVolumeClaim{Namespace: testNS, Name: testName + "-cache"}); err != nil {
		t.Fatal(err)
	}

	if err := r.Preflight(context.Background(), job); err != nil {
		t.Errorf("preflight should pass with dependencies present: %v", err)
	}
}

func TestBlockedReasonUnschedulable(t *testing.T) {
	pod := &corev1.Pod{Status: corev1.PodStatus{
		Conditions: []corev1.PodCondition{{
			Type: corev1.PodScheduled, Status: corev1.ConditionFalse,
			Reason:  corev1.PodReasonUnschedulable,
			Message: "0/3 nodes are available: 3 node(s) had volume node affinity conflict.",
		}},
	}}
	if got := blockedReason(pod); !strings.Contains(got, "Unschedulable") {
		t.Errorf("blockedReason = %q", got)
	}
}

func TestBuildOnlyMountsDatabaseCredentialsWhenAsked(t *testing.T) {
	mutate := func(sb *borgbasev1.ScheduledBackup) {
		sb.Spec.Database = &borgbasev1.DatabaseSpec{
			Engine: borgbasev1.DatabaseEngineCNPG, Host: "postgresql-rw",
			Name: testDBName, User: testDBName,
		}
	}
	r, sb := fixture(t, mutate)
	ctx := context.Background()

	bare, err := r.Build(ctx, sb, Options{Command: testCommand})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if hasVolume(bare, dbVolume) {
		t.Error("database credentials mounted into a run that does not use them")
	}

	restore, err := r.Build(ctx, sb, Options{Command: testCommand, MountDatabase: true})
	if err != nil {
		t.Fatalf("Build: %v", err)
	}
	if !hasVolume(restore, dbVolume) {
		t.Error("--to-database needs the credentials mounted")
	}
}

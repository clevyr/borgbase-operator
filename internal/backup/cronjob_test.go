package backup

import (
	"slices"
	"strings"
	"testing"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/healthchecks"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/utils/ptr"
)

const (
	testNS       = "myapp-prod"
	testSchedule = "4 * * * *"
	testDBName   = "appdb"
)

func testConfig() Config {
	return Config{
		Image:             "ghcr.io/clevyr/restic:test",
		CacheStorageClass: "nfs-regional-ssd",
		Healthchecks: healthchecks.Config{
			Enabled:    true,
			APIURL:     "http://healthchecks.healthchecks:8000/ping",
			AutoCreate: true,
		},
	}
}

func testRepo() *borgbasev1.Repository {
	return &borgbasev1.Repository{
		Name: containerName, Namespace: testNS,
	}
}

func testBackup(mutate func(*borgbasev1.ScheduledBackup)) *borgbasev1.ScheduledBackup {
	sb := &borgbasev1.ScheduledBackup{
		Name: containerName, Namespace: testNS,
		Spec: borgbasev1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: containerName},
			Schedule:      testSchedule,
			TimeZone:      "America/Chicago",
			Sources:       []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
			Healthchecks: &borgbasev1.HealthchecksSpec{
				PingKeySecretRef: &corev1.SecretKeySelector{
					Name: "healthchecks-ping-key",
					Key:  "PING_KEY",
				},
			},
		},
	}
	if mutate != nil {
		mutate(sb)
	}
	return sb
}

func envOf(c corev1.Container, name string) *corev1.EnvVar {
	for i := range c.Env {
		if c.Env[i].Name == name {
			return &c.Env[i]
		}
	}
	return nil
}

func containerOf(t *testing.T, sb *borgbasev1.ScheduledBackup, cfg Config) corev1.Container {
	t.Helper()
	cj, err := BuildCronJob(sb, testRepo(), cfg)
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	return cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
}

func TestBuildCronJobBasics(t *testing.T) {
	cj, err := BuildCronJob(testBackup(nil), testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	if cj.Spec.Schedule != testSchedule {
		t.Errorf("schedule = %q, want it passed through verbatim", cj.Spec.Schedule)
	}
	if got := ptr.Deref(cj.Spec.TimeZone, ""); got != "America/Chicago" {
		t.Errorf("timeZone = %q", got)
	}
	// The credentials come from the Repository's Secret, whole, so restic picks
	// RESTIC_REPOSITORY and RESTIC_PASSWORD straight out of the environment.
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if c.EnvFrom[0].SecretRef.Name != "restic-borgbase" {
		t.Errorf("envFrom secret = %q", c.EnvFrom[0].SecretRef.Name)
	}
	// A failed backup should wait for its next slot rather than retrying
	// against a repository the failed attempt may have left locked.
	if got := ptr.Deref(cj.Spec.JobTemplate.Spec.BackoffLimit, -1); got != 0 {
		t.Errorf("backoffLimit = %d, want 0", got)
	}
	if env := envOf(c, "RESTIC_HOST"); env == nil || env.ValueFrom == nil ||
		env.ValueFrom.FieldRef.FieldPath != "metadata.namespace" {
		t.Error("RESTIC_HOST must come from the downward API")
	}
}

// The cache volume must land on the path the image already points
// RESTIC_CACHE_DIR at, or the cache silently does nothing.
func TestCacheMountsWhereTheImageExpects(t *testing.T) {
	c := containerOf(t, testBackup(nil), testConfig())
	var found bool
	for _, m := range c.VolumeMounts {
		if m.Name == cacheVolume {
			found = true
			if m.MountPath != "/cache" {
				t.Errorf("cache mounted at %q, want /cache", m.MountPath)
			}
		}
	}
	if !found {
		t.Error("no cache volume mount")
	}

	pvc, err := BuildCachePVC(testBackup(nil), testConfig())
	if err != nil {
		t.Fatal(err)
	}
	if pvc.Name != containerName+"-cache" {
		t.Errorf("cache pvc name = %q", pvc.Name)
	}
	if ptr.Deref(pvc.Spec.StorageClassName, "") != "nfs-regional-ssd" {
		t.Errorf("cache storage class = %v", pvc.Spec.StorageClassName)
	}
}

func TestHealthchecksSlugMode(t *testing.T) {
	c := containerOf(t, testBackup(nil), testConfig())

	if c.Command[0] != "runitor" {
		t.Fatalf("command = %v, want it wrapped in runitor", c.Command)
	}
	if !slices.Contains(c.Command, "-create") {
		t.Error("expected -create so the check auto-provisions on first ping")
	}
	if env := envOf(c, "CHECK_SLUG"); env == nil || env.Value != "myapp-prod" {
		t.Errorf("CHECK_SLUG = %v, want the namespace", env)
	}
	if env := envOf(c, "PING_KEY"); env == nil || env.ValueFrom == nil ||
		env.ValueFrom.SecretKeyRef.Name != "healthchecks-ping-key" {
		t.Error("PING_KEY must come from the per-resource secret ref")
	}
	if envOf(c, "CHECK_UUID") != nil {
		t.Error("slug mode must not set CHECK_UUID")
	}
}

func TestHealthchecksEnabledWithoutKeyIsAnError(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) { s.Spec.Healthchecks = nil })
	if _, err := BuildCronJob(sb, testRepo(), testConfig()); err == nil {
		t.Fatal("expected an error when healthchecks is enabled with no ping key")
	}
}

func TestMariaDBWiring(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) {
		s.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeMariaDB}}
		s.Spec.Database = &borgbasev1.DatabaseSpec{
			Engine: borgbasev1.DatabaseEngineMariaDB,
			Host:   mariadbName, Name: testDBName, User: testDBName,
		}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]

	for name, want := range map[string]string{
		"DB_HOST": mariadbName, "DB_DATABASE": testDBName, "DB_USERNAME": testDBName,
	} {
		if env := envOf(c, name); env == nil || env.Value != want {
			t.Errorf("%s = %v, want %q", name, env, want)
		}
	}
	// MariaDB network policies select clients by this label.
	if cj.Spec.JobTemplate.Spec.Template.Labels["mariadb-client"] != "true" {
		t.Error("missing the mariadb-client pod label")
	}
	aff := cj.Spec.JobTemplate.Spec.Template.Spec.Affinity
	if aff == nil || aff.PodAffinity == nil ||
		len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("expected hard pod affinity to mariadb")
	}
}

// Missing connection details would produce a backup that fails at run time
// rather than at apply time.
func TestMariaDBWithoutConnectionDetailsIsAnError(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) {
		s.Spec.Database = &borgbasev1.DatabaseSpec{Engine: borgbasev1.DatabaseEngineMariaDB}
	})
	if _, err := BuildCronJob(sb, testRepo(), testConfig()); err == nil {
		t.Fatal("expected an error for mariadb with no host, name or user")
	}
}

func TestCNPGUsesSoftAffinityAndNoDBEnv(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) {
		s.Spec.Database = &borgbasev1.DatabaseSpec{Engine: borgbasev1.DatabaseEngineCNPG}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	c := cj.Spec.JobTemplate.Spec.Template.Spec.Containers[0]
	if envOf(c, "DB_HOST") != nil {
		t.Error("cnpg reads its credentials from the mounted secret, not DB_* env")
	}
	aff := cj.Spec.JobTemplate.Spec.Template.Spec.Affinity
	// Soft, so a failover cannot leave the backup unschedulable.
	if aff == nil || aff.PodAffinity == nil ||
		len(aff.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("expected soft pod affinity to the cnpg primary")
	}
	if len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 0 {
		t.Error("cnpg affinity must not be a hard constraint")
	}
}

// A ReadWriteOnce claim can only be mounted where the app already has it, so
// an attached volume must win over the database's softer preference.
func TestVolumeAffinityWinsOverDatabase(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) {
		s.Spec.Database = &borgbasev1.DatabaseSpec{Engine: borgbasev1.DatabaseEngineCNPG}
		s.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "myapp-prod-storage"}
	})
	cj, err := BuildCronJob(sb, testRepo(), testConfig())
	if err != nil {
		t.Fatalf("BuildCronJob() error = %v", err)
	}
	spec := cj.Spec.JobTemplate.Spec.Template.Spec
	aff := spec.Affinity
	if aff == nil || aff.PodAffinity == nil ||
		len(aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution) != 1 {
		t.Fatal("expected hard pod affinity to the app when a claim is attached")
	}
	sel := aff.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution[0].LabelSelector
	if sel.MatchLabels[labelName] != testNS {
		t.Errorf("affinity selector = %v", sel.MatchLabels)
	}

	c := spec.Containers[0]
	if c.WorkingDir != "/myapp-prod-storage" {
		t.Errorf("workingDir = %q, want the claim mount path", c.WorkingDir)
	}
}

func TestSpecImageOverridesConfig(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) { s.Spec.Image = "custom:1.2.3" })
	if got := containerOf(t, sb, testConfig()).Image; got != "custom:1.2.3" {
		t.Errorf("image = %q", got)
	}
}

// Env is rendered from a map, so it must be sorted or every reconcile would
// see a spurious diff and rewrite the CronJob.
func TestUserEnvIsSorted(t *testing.T) {
	sb := testBackup(func(s *borgbasev1.ScheduledBackup) {
		s.Spec.Env = map[string]string{"ZED": "1", "ALPHA": "2", "MIKE": "3"}
	})
	for range 20 {
		c := containerOf(t, sb, testConfig())
		var got []string
		for _, e := range c.Env {
			if e.Name == "ZED" || e.Name == "ALPHA" || e.Name == "MIKE" {
				got = append(got, e.Name)
			}
		}
		if strings.Join(got, ",") != "ALPHA,MIKE,ZED" {
			t.Fatalf("env order = %v, want sorted", got)
		}
	}
}

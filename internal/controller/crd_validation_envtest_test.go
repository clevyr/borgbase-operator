//go:build envtest

package controller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/yaml"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func startEnv(t *testing.T) client.Client {
	t.Helper()
	env := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}

	cfg, err := env.Start()
	if err != nil {
		t.Fatalf("starting envtest (CRDs may have an invalid CEL rule): %v", err)
	}
	t.Cleanup(func() { _ = env.Stop() })

	c, err := client.New(cfg, client.Options{Scheme: testScheme(t)})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func mustCreateNamespace(t *testing.T, c client.Client, name string) {
	t.Helper()
	if err := c.Create(context.Background(), &corev1.Namespace{Name: name}); err != nil {
		t.Fatal(err)
	}
}

func TestCRDRejectsUnparseableDurations(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "durations")

	tests := []struct {
		value   string
		wantErr bool
	}{
		{"1h", false},
		{"90m", false},
		{"1h30m", false},
		{"0", false},
		{"1 hour", true},
		{"hourly", true},
		{"1d", true},
		{"", true},
	}

	for i, tt := range tests {
		obj := repositoryWithRawInterval(fmt.Sprintf("dur-%d", i), "durations", tt.value)
		err := c.Create(context.Background(), obj)
		if tt.wantErr && err == nil {
			t.Errorf("interval %q was accepted, want rejection", tt.value)
		}
		if !tt.wantErr && err != nil {
			t.Errorf("interval %q was rejected: %v", tt.value, err)
		}
	}
}

func TestCRDExistingRepositoryIDIsImmutable(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "immutable")

	adopted := &borgbasev1.Repository{
		Name: "adopted", Namespace: "immutable",
		Spec: borgbasev1.RepositorySpec{
			ExistingRepositoryID: "a1b2c3d4",
			PasswordSecretRef:    &corev1.SecretKeySelector{Name: "seed", Key: "RESTIC_PASSWORD"},
		},
	}
	if err := c.Create(ctx, adopted); err != nil {
		t.Fatal(err)
	}

	t.Run("cannot be changed", func(t *testing.T) {
		var got borgbasev1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(adopted), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = "e5f6a7b8"
		if err := c.Update(ctx, &got); err == nil {
			t.Error("existingRepositoryID was changed")
		}
	})

	t.Run("cannot be removed", func(t *testing.T) {
		var got borgbasev1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(adopted), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = ""
		if err := c.Update(ctx, &got); err == nil {
			t.Error("existingRepositoryID was removed, which the old field-level rule allowed")
		}
	})

	t.Run("cannot be added after a repository was created", func(t *testing.T) {
		created := &borgbasev1.Repository{
			Name: "created", Namespace: "immutable",
			Spec: borgbasev1.RepositorySpec{Region: "us"},
		}
		if err := c.Create(ctx, created); err != nil {
			t.Fatal(err)
		}
		created.Status.RepositoryID = "zzzzzzzz"
		if err := c.Status().Update(ctx, created); err != nil {
			t.Fatal(err)
		}

		var got borgbasev1.Repository
		if err := c.Get(ctx, client.ObjectKeyFromObject(created), &got); err != nil {
			t.Fatal(err)
		}
		got.Spec.ExistingRepositoryID = "a1b2c3d4"
		got.Spec.PasswordSecretRef = &corev1.SecretKeySelector{Name: "seed", Key: "RESTIC_PASSWORD"}
		if err := c.Update(ctx, &got); err == nil {
			t.Error("adopted a different repository after one had already been created")
		}
	})
}

func TestCRDRejectsSecretNameEqualToSeed(t *testing.T) {
	c := startEnv(t)
	mustCreateNamespace(t, c, "seed")

	repo := &borgbasev1.Repository{
		Name: "clash", Namespace: "seed",
		Spec: borgbasev1.RepositorySpec{
			ExistingRepositoryID: "a1b2c3d4",
			PasswordSecretRef:    &corev1.SecretKeySelector{Name: "restic-envs", Key: "RESTIC_PASSWORD"},
			SecretName:           "restic-envs",
		},
	}
	if err := c.Create(context.Background(), repo); err == nil {
		t.Error("secretName was allowed to name the seed Secret")
	}

	repo.Name = "fine"
	repo.Spec.SecretName = "restic-borgbase"
	if err := c.Create(context.Background(), repo); err != nil {
		t.Errorf("a distinct secretName was rejected: %v", err)
	}
}

func TestCRDScheduledBackupValidation(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "backups")

	base := func() *borgbasev1.ScheduledBackup {
		return &borgbasev1.ScheduledBackup{
			Namespace: "backups",
			Spec: borgbasev1.ScheduledBackupSpec{
				RepositoryRef: corev1.LocalObjectReference{Name: "restic"},
				Schedule:      "@hourly",
				Sources:       []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
			},
		}
	}

	tests := []struct {
		name    string
		mutate  func(*borgbasev1.ScheduledBackup)
		wantErr bool
	}{
		{
			name: "files source without a volume",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeFiles}}
			},
			wantErr: true,
		},
		{
			name: "files source with a volume",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Sources = []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeFiles}}
				s.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "data"}
			},
		},
		{
			name:    "no sources and no script",
			mutate:  func(s *borgbasev1.ScheduledBackup) { s.Spec.Sources = nil },
			wantErr: true,
		},
		{
			name:    "empty sources list",
			mutate:  func(s *borgbasev1.ScheduledBackup) { s.Spec.Sources = []borgbasev1.BackupSource{} },
			wantErr: true,
		},
		{
			name:    "retention with nothing set",
			mutate:  func(s *borgbasev1.ScheduledBackup) { s.Spec.Retention = &borgbasev1.Retention{} },
			wantErr: true,
		},
		{
			name: "retention with one tier",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Retention = &borgbasev1.Retention{Daily: ptr.To(int32(90))}
			},
		},
		{
			name: "mariadb without connection details",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Database = &borgbasev1.DatabaseSpec{Engine: borgbasev1.DatabaseEngineMariaDB}
			},
			wantErr: true,
		},
		{
			name: "mariadb with connection details",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Database = &borgbasev1.DatabaseSpec{
					Engine: borgbasev1.DatabaseEngineMariaDB,
					Host:   "mariadb", Name: "appdb", User: "appdb",
				}
			},
		},
		{
			name: "exclude on a database source",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Sources = []borgbasev1.BackupSource{
					{Type: borgbasev1.SourceTypeCNPG, Exclude: []string{"nope"}},
				}
			},
			wantErr: true,
		},
		{
			name: "database name on a files source",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Sources = []borgbasev1.BackupSource{
					{Type: borgbasev1.SourceTypeFiles, Database: "nope"},
				}
				s.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "data"}
			},
			wantErr: true,
		},
		{
			name: "volume mounted over the cache",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "data", MountPath: "/cache"}
			},
			wantErr: true,
		},
		{
			name: "unknown concurrency policy",
			mutate: func(s *borgbasev1.ScheduledBackup) {
				s.Spec.ConcurrencyPolicy = "Sometimes"
			},
			wantErr: true,
		},
	}

	for i, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sb := base()
			sb.Name = "sb" + string(rune('a'+i))
			tt.mutate(sb)

			err := c.Create(ctx, sb)
			if tt.wantErr && err == nil {
				t.Error("accepted, want rejection")
			}
			if !tt.wantErr && err != nil {
				t.Errorf("rejected: %v", err)
			}
		})
	}
}

func repositoryWithRawInterval(name, namespace, interval string) client.Object {
	obj := &unstructured.Unstructured{}
	obj.SetUnstructuredContent(map[string]any{
		"apiVersion": "borgbase.clevyr.com/v1",
		"kind":       "Repository",
		"metadata": map[string]any{
			"name":      name,
			"namespace": namespace,
		},
		"spec": map[string]any{
			"interval": interval,
		},
	})
	return obj
}

func TestCRDSamplesAreValid(t *testing.T) {
	c := startEnv(t)
	ctx := context.Background()
	mustCreateNamespace(t, c, "myapp-prod")
	mustCreateNamespace(t, c, "otherapp-prod")

	matches, err := filepath.Glob(filepath.Join("..", "..", "config", "samples", "borgbase_v1_*.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(matches) == 0 {
		t.Fatal("no sample manifests found")
	}

	for _, path := range matches {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		for i, doc := range strings.Split(string(raw), "\n---\n") {
			if strings.TrimSpace(doc) == "" {
				continue
			}
			obj := &unstructured.Unstructured{}
			if err := yaml.Unmarshal([]byte(doc), obj); err != nil {
				t.Errorf("%s doc %d: %v", filepath.Base(path), i, err)
				continue
			}
			if err := c.Create(ctx, obj); err != nil {
				t.Errorf("%s doc %d (%s/%s) was rejected: %v",
					filepath.Base(path), i, obj.GetNamespace(), obj.GetName(), err)
			}
		}
	}
}

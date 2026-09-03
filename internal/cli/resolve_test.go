package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func newClient(t *testing.T, objs ...client.Object) client.Client {
	t.Helper()
	s := runtime.NewScheme()
	if err := borgbasev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return fake.NewClientBuilder().WithScheme(s).WithObjects(objs...).Build()
}

func repo(ns, name string) *borgbasev1.Repository {
	return &borgbasev1.Repository{ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name}}
}

func backup(ns, name, repoRef string) *borgbasev1.ScheduledBackup {
	return &borgbasev1.ScheduledBackup{
		ObjectMeta: metav1.ObjectMeta{Namespace: ns, Name: name},
		Spec: borgbasev1.ScheduledBackupSpec{
			RepositoryRef: corev1.LocalObjectReference{Name: repoRef},
		},
	}
}

func TestResolve(t *testing.T) {
	c := newClient(t, repo("prod", "store"), backup("prod", "web-files", "store"))
	ctx := context.Background()

	tests := []struct {
		name     string
		arg      string
		wantKind TargetKind
		wantName string
	}{
		{"bare name finds the backup", "web-files", TargetScheduledBackup, "web-files"},
		{"bare name finds the repository", "store", TargetRepository, "store"},
		{"sb prefix", "sb/web-files", TargetScheduledBackup, "web-files"},
		{"scheduledbackup prefix", "scheduledbackup/web-files", TargetScheduledBackup, "web-files"},
		{"backup prefix", "backup/web-files", TargetScheduledBackup, "web-files"},
		{"repo prefix", "repo/store", TargetRepository, "store"},
		{"repository prefix", "repository/store", TargetRepository, "store"},
		{"prefixes are case insensitive", "SB/web-files", TargetScheduledBackup, "web-files"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := Resolve(ctx, c, "prod", tt.arg)
			if err != nil {
				t.Fatalf("Resolve(%q): %v", tt.arg, err)
			}
			if got.Kind != tt.wantKind {
				t.Errorf("kind = %q, want %q", got.Kind, tt.wantKind)
			}
			if got.Name() != tt.wantName {
				t.Errorf("name = %q, want %q", got.Name(), tt.wantName)
			}
			if got.Namespace() != "prod" {
				t.Errorf("namespace = %q, want %q", got.Namespace(), "prod")
			}
		})
	}
}

// A bare name that matches both kinds must not be guessed at, since the two
// commands it could dispatch to do very different things.
func TestResolveRejectsAmbiguousName(t *testing.T) {
	c := newClient(t, repo("prod", "store"), backup("prod", "store", "store"))

	_, err := Resolve(context.Background(), c, "prod", "store")
	if !errors.Is(err, ErrAmbiguous) {
		t.Fatalf("expected ErrAmbiguous, got %v", err)
	}
	for _, want := range []string{"sb/store", "repo/store"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error should suggest %q, got: %v", want, err)
		}
	}
}

func TestResolveErrors(t *testing.T) {
	c := newClient(t, repo("prod", "store"))
	ctx := context.Background()

	for _, tt := range []struct {
		name string
		arg  string
		want error
	}{
		{"missing object", "nope", ErrTargetNotFound},
		{"missing qualified object", "repo/nope", ErrTargetNotFound},
		{"wrong kind for an existing name", "sb/store", ErrTargetNotFound},
		{"unknown prefix", "cronjob/store", ErrUnknownKind},
		{"empty name after prefix", "repo/", ErrTargetNotFound},
		{"empty arg", "", ErrTargetNotFound},
	} {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := Resolve(ctx, c, "prod", tt.arg); !errors.Is(err, tt.want) {
				t.Errorf("Resolve(%q) = %v, want %v", tt.arg, err, tt.want)
			}
		})
	}
}

func TestResolveIsNamespaceScoped(t *testing.T) {
	c := newClient(t, repo("prod", "store"))
	if _, err := Resolve(context.Background(), c, "staging", "store"); !errors.Is(err, ErrTargetNotFound) {
		t.Errorf("expected ErrTargetNotFound in another namespace, got %v", err)
	}
}

func TestRepositoryFor(t *testing.T) {
	sb := backup("prod", "web-files", "store")
	c := newClient(t, repo("prod", "store"), sb)

	got, err := RepositoryFor(context.Background(), c, sb)
	if err != nil {
		t.Fatalf("RepositoryFor: %v", err)
	}
	if got.Name != "store" {
		t.Errorf("repository = %q, want %q", got.Name, "store")
	}

	// A dangling reference is the RepositoryNotFound path the operator reports,
	// so the message has to name both objects.
	dangling := backup("prod", "orphan", "gone")
	_, err = RepositoryFor(context.Background(), newClient(t, dangling), dangling)
	if !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expected ErrTargetNotFound, got %v", err)
	}
	if !strings.Contains(err.Error(), "orphan") || !strings.Contains(err.Error(), "gone") {
		t.Errorf("error should name both objects, got: %v", err)
	}
}

package cli

import (
	"archive/tar"
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func withVolume(sb *borgbasev1.ScheduledBackup) {
	sb.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: testClaimName, MountPath: "/app"}
}

func withDatabase(sb *borgbasev1.ScheduledBackup) {
	sb.Spec.Database = &borgbasev1.DatabaseSpec{
		Engine: borgbasev1.DatabaseEngineCNPG, Host: "postgresql-rw", Name: "app", User: "app",
	}
}

func TestRestoreRefusesTwoTargets(t *testing.T) {
	o := &restoreOptions{inPlace: true, toDatabase: true}
	if got := o.targets(); len(got) != 2 {
		t.Fatalf("targets = %v", got)
	}

	sb := newBackup(testBackupName, testRepoName)
	repo := newRepo(testNS)
	f := &Factory{Streams: testStreams()}
	err := runRestore(context.Background(), f, newClient(t, sb, repo), sb, o)
	if err == nil || !strings.Contains(err.Error(), "choose one restore target") {
		t.Fatalf("expected a single-target error, got %v", err)
	}
}

// Without a terminal a script must never restore somewhere it did not name.
func TestRestoreWithoutATargetIsRefusedNonInteractively(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	withDatabase(sb)
	repo := newRepo(testNS)

	var errOut bytes.Buffer
	f := &Factory{Streams: testStreams()}
	f.Streams.ErrOut = &errOut

	err := runRestore(context.Background(), f, newClient(t, sb, repo), sb, &restoreOptions{})
	if !errors.Is(err, ErrNoTarget) {
		t.Fatalf("expected ErrNoTarget, got %v", err)
	}
	// It must list what is actually possible for this backup.
	for _, want := range []string{"--to DIR", "--to-new-pvc", "--in-place", "--to-database", testClaimName} {
		if !strings.Contains(errOut.String(), want) {
			t.Errorf("expected %q in the target list:\n%s", want, errOut.String())
		}
	}
}

// A backup with no volume must not be offered volume targets.
func TestAvailableTargetsMatchTheBackup(t *testing.T) {
	dbOnly := newBackup(testBackupName, testRepoName)
	withDatabase(dbOnly)
	got := strings.Join(availableTargets(dbOnly), " ")
	if strings.Contains(got, "--in-place") || strings.Contains(got, "--to-new-pvc") {
		t.Errorf("a database-only backup was offered volume targets: %s", got)
	}
	if !strings.Contains(got, "--to-database") {
		t.Errorf("a database backup should offer --to-database: %s", got)
	}

	filesOnly := newBackup(testBackupName, testRepoName)
	withVolume(filesOnly)
	got = strings.Join(availableTargets(filesOnly), " ")
	if strings.Contains(got, "--to-database") {
		t.Errorf("a files-only backup was offered --to-database: %s", got)
	}
}

func TestRestoreRejectsMissingSources(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	repo := newRepo(testNS)
	c := newClient(t, sb, repo)
	f := &Factory{Streams: testStreams()}
	ctx := context.Background()

	if err := runRestore(ctx, f, c, sb, &restoreOptions{inPlace: true, yes: true}); !errors.Is(err, ErrNoSourceVolume) {
		t.Errorf("expected ErrNoSourceVolume, got %v", err)
	}
	if err := runRestore(ctx, f, c, sb, &restoreOptions{toDatabase: true, yes: true}); !errors.Is(err, ErrNoDatabase) {
		t.Errorf("expected ErrNoDatabase, got %v", err)
	}
}

func TestConfirmRequiresTheExactName(t *testing.T) {
	f := &Factory{Streams: testStreams()}
	f.Streams.In = strings.NewReader("wrong-name\n")
	if err := confirm(f, &restoreOptions{}, "pvc/app-data", testBackupName); !errors.Is(err, ErrNotConfirmed) {
		t.Errorf("expected ErrNotConfirmed, got %v", err)
	}

	f.Streams.In = strings.NewReader(testBackupName + "\n")
	if err := confirm(f, &restoreOptions{}, "pvc/app-data", testBackupName); err != nil {
		t.Errorf("the exact name should confirm, got %v", err)
	}

	// --yes and --dry-run skip the prompt.
	f.Streams.In = strings.NewReader("")
	if err := confirm(f, &restoreOptions{yes: true}, "x", "y"); err != nil {
		t.Errorf("--yes should skip confirmation, got %v", err)
	}
	if err := confirm(f, &restoreOptions{dryRun: true}, "x", "y"); err != nil {
		t.Errorf("--dry-run should skip confirmation, got %v", err)
	}
}

func TestResticRestoreArgs(t *testing.T) {
	o := &restoreOptions{
		snapshot: "4f2a1b0c",
		path:     "app/uploads",
		exclude:  []string{"**/cache"},
		delete:   true,
	}
	got := strings.Join(o.resticRestoreArgs("/app"), " ")
	for _, want := range []string{
		"restore 4f2a1b0c", "--target=/app", "--include=app/uploads", "--exclude=**/cache", "--delete",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("expected %q in %q", want, got)
		}
	}

	dry := (&restoreOptions{snapshot: "latest", dryRun: true}).resticRestoreArgs("/app")
	if !strings.Contains(strings.Join(dry, " "), "--dry-run") {
		t.Errorf("--dry-run was not passed through: %v", dry)
	}
}

// The extension must match what the backup image's wrapper used when it named
// the dump inside the snapshot, or the restore reads a file that is not there.
func TestDumpExtension(t *testing.T) {
	if got := dumpExtension(borgbasev1.DatabaseEngineCNPG); got != ".dmp" {
		t.Errorf("cnpg = %q, want .dmp", got)
	}
	if got := dumpExtension(borgbasev1.DatabaseEngineMariaDB); got != ".sql" {
		t.Errorf("mariadb = %q, want .sql", got)
	}
}

func TestClaimSize(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	withVolume(sb)
	source := &corev1.PersistentVolumeClaim{
		Namespace: testNS, Name: testClaimName,
		Spec: corev1.PersistentVolumeClaimSpec{
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: resource.MustParse("20Gi")},
			},
		},
	}
	c := newClient(t, sb, source)
	ctx := context.Background()

	// Inherited from the claim being restored, so the copy is sure to fit.
	got, err := claimSize(ctx, c, sb, "")
	if err != nil {
		t.Fatalf("claimSize: %v", err)
	}
	if got.String() != "20Gi" {
		t.Errorf("size = %s, want 20Gi", got.String())
	}

	if got, err = claimSize(ctx, c, sb, "50Gi"); err != nil || got.String() != "50Gi" {
		t.Errorf("--size override = %s, %v", got.String(), err)
	}

	// A missing source claim must say how to proceed.
	missing := newBackup("other", testRepoName)
	missing.Spec.Volume = &borgbasev1.VolumeSpec{ExistingClaim: "gone"}
	if _, err := claimSize(ctx, c, missing, ""); err == nil || !strings.Contains(err.Error(), "--size") {
		t.Errorf("expected a hint to pass --size, got %v", err)
	}
}

func TestUntar(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	write := func(name, body string) {
		if err := tw.WriteHeader(&tar.Header{
			Name: name, Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := io.WriteString(tw, body); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.WriteHeader(&tar.Header{Name: "app/", Mode: 0o755, Typeflag: tar.TypeDir}); err != nil {
		t.Fatal(err)
	}
	write("app/config.php", "<?php")
	write("app/uploads/a.txt", "hello")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	n, err := untar(&buf, dest)
	if err != nil {
		t.Fatalf("untar: %v", err)
	}
	if n != 2 {
		t.Errorf("restored %d files, want 2", n)
	}
	body, err := os.ReadFile(filepath.Join(dest, "app/uploads/a.txt"))
	if err != nil || string(body) != "hello" {
		t.Errorf("file content = %q, %v", body, err)
	}
}

// A snapshot is not necessarily trusted input; an entry must not escape the
// directory the user named.
func TestUntarRefusesPathTraversal(t *testing.T) {
	var buf bytes.Buffer
	tw := tar.NewWriter(&buf)
	body := "pwned"
	if err := tw.WriteHeader(&tar.Header{
		Name: "../../escape.txt", Mode: 0o644, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.WriteString(tw, body); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}

	dest := t.TempDir()
	if _, err := untar(&buf, dest); err != nil {
		// Cleaning the name to /escape.txt keeps it inside dest, which is the
		// safe outcome; an error is equally acceptable.
		if !strings.Contains(err.Error(), "outside the target directory") {
			t.Fatalf("unexpected error: %v", err)
		}
		return
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(dest), "escape.txt")); err == nil {
		t.Fatal("a tar entry escaped the target directory")
	}
}

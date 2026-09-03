package cli

import (
	"context"
	"errors"
	"strings"
	"testing"

	"k8s.io/utils/ptr"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func TestSourceTags(t *testing.T) {
	sb := newBackup(testBackupName, testRepoName)
	sb.Spec.Sources = []borgbasev1.BackupSource{
		{Type: borgbasev1.SourceTypeCNPG},
		{Type: borgbasev1.SourceTypeFiles},

		{Type: borgbasev1.SourceTypeCNPG, Tag: testDBTag},

		{Type: borgbasev1.SourceTypeFiles},
	}

	got := sourceTags(sb)
	want := []string{"db", "files", testDBTag}
	if strings.Join(got, ",") != strings.Join(want, ",") {
		t.Errorf("sourceTags = %v, want %v", got, want)
	}
}

func TestRetentionFlags(t *testing.T) {
	if got := retentionFlags(nil); got != nil {
		t.Errorf("retentionFlags(nil) = %v, want nil", got)
	}
	if got := retentionFlags(&borgbasev1.Retention{}); got != nil {
		t.Errorf("an empty policy should produce no flags, got %v", got)
	}

	got := retentionFlags(&borgbasev1.Retention{
		Hourly:  ptr.To(int32(168)),
		Daily:   ptr.To(int32(90)),
		Monthly: ptr.To(int32(24)),
		Yearly:  ptr.To(int32(10)),
	})
	want := "--keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10"
	if strings.Join(got, " ") != want {
		t.Errorf("retentionFlags =\n %v\nwant\n %v", strings.Join(got, " "), want)
	}
}

func TestResolveRunTargetFindsABackupForARepository(t *testing.T) {
	repo := newRepo(testNS)
	sb := newBackup(testBackupName, testRepoName)
	c := newClient(t, repo, sb)

	gotSB, gotRepo, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName)
	if err != nil {
		t.Fatalf("resolveRunTarget: %v", err)
	}
	if gotSB.Name != testBackupName {
		t.Errorf("backup = %q, want %q", gotSB.Name, testBackupName)
	}
	if gotRepo.Name != testRepoName {
		t.Errorf("repository = %q, want %q", gotRepo.Name, testRepoName)
	}
}

func TestResolveRunTargetWithoutAnyBackup(t *testing.T) {
	c := newClient(t, newRepo(testNS))

	_, _, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName)
	if !errors.Is(err, ErrNoBackupForRepository) {
		t.Fatalf("expected ErrNoBackupForRepository, got %v", err)
	}
}

func TestResolveRunTargetIgnoresUnrelatedBackups(t *testing.T) {
	repo := newRepo(testNS)
	other := newBackup("other", "somewhere-else")
	c := newClient(t, repo, other)

	if _, _, err := resolveRunTarget(context.Background(), c, testNS, "repo/"+testRepoName); !errors.Is(err, ErrNoBackupForRepository) {
		t.Fatalf("expected ErrNoBackupForRepository, got %v", err)
	}
}

func TestResticCommand(t *testing.T) {
	got := resticCommand("snapshots", "--tag=files")
	if strings.Join(got, " ") != "restic snapshots --tag=files" {
		t.Errorf("resticCommand = %v", got)
	}
}

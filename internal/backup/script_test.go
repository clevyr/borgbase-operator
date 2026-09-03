package backup

import (
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"k8s.io/utils/ptr"
)

func hourlyRetention() *borgbasev1.Retention    { return retention(168) }
func sixHourlyRetention() *borgbasev1.Retention { return retention(28) }
func dailyRetention() *borgbasev1.Retention     { return retention(0) }

func retention(hourly int32) *borgbasev1.Retention {
	r := &borgbasev1.Retention{
		Daily:   ptr.To[int32](90),
		Monthly: ptr.To[int32](24),
		Yearly:  ptr.To[int32](10),
	}
	if hourly > 0 {
		r.Hourly = ptr.To(hourly)
	}
	return r
}

func TestRender(t *testing.T) {
	tests := []struct {
		name string
		spec borgbasev1.ScheduledBackupSpec
		want string
	}{
		{
			name: "cnpg and files with excludes",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources: []borgbasev1.BackupSource{
					{Type: borgbasev1.SourceTypeCNPG},
					{
						Type:    borgbasev1.SourceTypeFiles,
						Path:    "app",
						Exclude: []string{"**/temp*", "app/export-logs"},
					},
				},
				Retention: hourlyRetention(),
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/borgbase/database
restic backup --retry-lock=5m0s --tag=files app \
  --exclude='**/temp*' \
  --exclude='app/export-logs'
restic forget --prune --retry-lock=5m0s --keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
`,
		},
		{
			name: "mariadb with extra args",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources: []borgbasev1.BackupSource{
					{Type: borgbasev1.SourceTypeMariaDB, ExtraArgs: []string{"--skip-ssl"}},
					{Type: borgbasev1.SourceTypeFiles, Exclude: []string{"dumps"}},
				},
				Retention: dailyRetention(),
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb mariadb --secret-mount=/var/run/secrets/borgbase/database -- --skip-ssl
restic backup --retry-lock=5m0s --tag=files . \
  --exclude='dumps'
restic forget --prune --retry-lock=5m0s --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
`,
		},
		{
			name: "named database with custom tag",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources: []borgbasev1.BackupSource{
					{Type: borgbasev1.SourceTypeCNPG},
					{Type: borgbasev1.SourceTypeCNPG, Tag: "db-external", Database: "reporting"},
				},
				Retention: dailyRetention(),
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/borgbase/database
restic backup --retry-lock=5m0s --tag=db-external --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/borgbase/database --database=reporting
restic forget --prune --retry-lock=5m0s --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
`,
		},
		{
			name: "files only without excludes",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources:   []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeFiles}},
				Retention: hourlyRetention(),
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=files .
restic forget --prune --retry-lock=5m0s --keep-hourly=168 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
`,
		},
		{
			name: "six hourly retention tier",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources:   []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
				Retention: sixHourlyRetention(),
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/borgbase/database
restic forget --prune --retry-lock=5m0s --keep-hourly=28 --keep-daily=90 --keep-monthly=24 --keep-yearly=10
restic cache --cleanup
`,
		},
		{
			name: "raw script keeps preamble and postamble",
			spec: borgbasev1.ScheduledBackupSpec{
				Script: "restic backup --retry-lock=5m0s --tag=custom /data\n",
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=custom /data
restic cache --cleanup
`,
		},
		{
			name: "no retention omits forget entirely",
			spec: borgbasev1.ScheduledBackupSpec{
				Sources: []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
			},
			want: `exec > >(ts '%H:%M:%S') 2>&1
set -eu
restic backup --retry-lock=5m0s --tag=db --stdin-from-command -- dumpdb cnpg --secret-mount=/var/run/secrets/borgbase/database
restic cache --cleanup
`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.spec.RetryLock == nil {
				tt.spec.RetryLock = &metav1.Duration{Duration: 5 * time.Minute}
			}
			got, err := Render(&tt.spec)
			if err != nil {
				t.Fatalf("Render() error = %v", err)
			}
			if got != tt.want {
				t.Errorf("Render() mismatch\n--- got ---\n%s\n--- want ---\n%s", got, tt.want)
			}
		})
	}
}

func TestRenderUnknownSourceType(t *testing.T) {
	_, err := Render(&borgbasev1.ScheduledBackupSpec{
		Sources: []borgbasev1.BackupSource{{Type: "nope"}},
	})
	if err == nil {
		t.Fatal("Render() expected an error for an unknown source type")
	}
}

func TestShellQuoteEscapesSingleQuotes(t *testing.T) {
	got, err := Render(&borgbasev1.ScheduledBackupSpec{
		Sources: []borgbasev1.BackupSource{
			{Type: borgbasev1.SourceTypeFiles, Exclude: []string{`it's; rm -rf /`}},
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	want := `  --exclude='it'\''s; rm -rf /'`
	if !strings.Contains(got, want) {
		t.Errorf("Render() did not escape the quote\n--- got ---\n%s\n--- want line ---\n%s", got, want)
	}
}

func TestRetryLockIsAppliedToLockingCommands(t *testing.T) {
	spec := borgbasev1.ScheduledBackupSpec{
		Sources: []borgbasev1.BackupSource{
			{Type: borgbasev1.SourceTypeCNPG},
			{Type: borgbasev1.SourceTypeFiles},
		},
		Retention: hourlyRetention(),
		RetryLock: &metav1.Duration{Duration: 90 * time.Second},
	}
	got, err := Render(&spec)
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	for _, want := range []string{
		"restic backup --retry-lock=1m30s --tag=db",
		"restic backup --retry-lock=1m30s --tag=files",
		"restic forget --prune --retry-lock=1m30s --keep-hourly=168",
	} {
		if !strings.Contains(got, want) {
			t.Errorf("Render() missing %q\n--- got ---\n%s", want, got)
		}
	}
}

func TestRetryLockCanBeDisabled(t *testing.T) {
	for _, d := range []*metav1.Duration{nil, {Duration: 0}} {
		spec := borgbasev1.ScheduledBackupSpec{
			Sources:   []borgbasev1.BackupSource{{Type: borgbasev1.SourceTypeCNPG}},
			Retention: hourlyRetention(),
			RetryLock: d,
		}
		got, err := Render(&spec)
		if err != nil {
			t.Fatalf("Render() error = %v", err)
		}
		if strings.Contains(got, "--retry-lock") {
			t.Errorf("Render() emitted --retry-lock for %v:\n%s", d, got)
		}
	}
}

func TestOrdinaryPathsRenderUnquoted(t *testing.T) {
	got, err := Render(&borgbasev1.ScheduledBackupSpec{
		Sources: []borgbasev1.BackupSource{
			{Type: borgbasev1.SourceTypeFiles, Path: "app/storage"},
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, "--tag=files app/storage\n") {
		t.Errorf("Render() quoted an ordinary path:\n%s", got)
	}
}

func TestPathWithSpaceIsQuoted(t *testing.T) {
	got, err := Render(&borgbasev1.ScheduledBackupSpec{
		Sources: []borgbasev1.BackupSource{
			{Type: borgbasev1.SourceTypeFiles, Path: "app/user uploads"},
		},
	})
	if err != nil {
		t.Fatalf("Render() error = %v", err)
	}
	if !strings.Contains(got, `--tag=files 'app/user uploads'`) {
		t.Errorf("Render() did not quote a path with a space:\n%s", got)
	}
}

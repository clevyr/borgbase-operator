package cli

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// A HelmRelease stub parity can read with yq.
func writeHelmRelease(t *testing.T, schedule string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "helmrelease.yaml")
	body := `apiVersion: helm.toolkit.fluxcd.io/v2
kind: HelmRelease
spec:
  values:
    controllers:
      restic:
        cronjob:
          schedule: "` + schedule + `"
          timeZone: America/Chicago
        containers:
          restic:
            command:
              - runitor
              - --
              - sh
              - -c
              - |
                exec > >(ts '%H:%M:%S') 2>&1
                set -eu
                restic backup --tag=db --stdin-from-command -- dumpdb cnpg
                restic cache --cleanup
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func writeGenerated(t *testing.T, schedule string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "generated.yaml")
	body := `apiVersion: borgbase.clevyr.com/v1
kind: ScheduledBackup
metadata:
  name: web-files
  namespace: prod
spec:
  repositoryRef:
    name: store
  schedule: "` + schedule + `"
  timeZone: America/Chicago
  sources:
    - type: cnpg
`
	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func parity(t *testing.T, generated, original string) (string, error) {
	t.Helper()
	var buf bytes.Buffer
	err := runParity(&buf, writeGenerated(t, generated), writeHelmRelease(t, original))
	return buf.String(), err
}

// Migration re-jitters the minute on purpose. That must not read as a
// difference, or every app reports one and the real ones are buried.
func TestParityAcceptsAReJitteredSchedule(t *testing.T) {
	out, err := parity(t, "@hourly", "36 * * * *")
	if err != nil {
		t.Fatalf("a re-jittered schedule should pass: %v\n%s", err, out)
	}
	for _, want := range []string{"RESCHEDULED", "EQUIVALENT"} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// But a real change in how often a backup runs must still fail.
func TestParityCatchesACadenceChange(t *testing.T) {
	out, err := parity(t, "@daily", "36 * * * *")
	if !errors.Is(err, ErrParityDiffers) {
		t.Fatalf("expected ErrParityDiffers, got %v\n%s", err, out)
	}
	if !strings.Contains(out, "CADENCE DIFFERS") {
		t.Errorf("expected CADENCE DIFFERS in output:\n%s", out)
	}
}

// A schedule left pinned verbatim reports IDENTICAL, not EQUIVALENT.
func TestParityIdenticalWhenNothingMoved(t *testing.T) {
	out, err := parity(t, "36 * * * *", "36 * * * *")
	if err != nil {
		t.Fatalf("unexpected difference: %v\n%s", err, out)
	}
	if !strings.Contains(out, "IDENTICAL") {
		t.Errorf("expected IDENTICAL in output:\n%s", out)
	}
	if strings.Contains(out, "RESCHEDULED") {
		t.Errorf("nothing moved, so nothing should be reported as rescheduled:\n%s", out)
	}
}

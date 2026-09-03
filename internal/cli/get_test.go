package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func readyRepo(ns, name string) *borgbasev1.Repository {
	r := newRepo(ns, name)
	r.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	r.Status = borgbasev1.RepositoryStatus{
		RepositoryID: "abcd1234",
		Initialized:  true,
		CurrentUsage: "2.1 TiB",
		Quota:        "4 TiB",
		Server:       "abcd1234.repo.borgbase.com",
		SecretName:   name + "-borgbase",
		Conditions: []metav1.Condition{
			{Type: borgbasev1.RepositoryConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	return r
}

func readyBackup(ns, name, repoRef string) *borgbasev1.ScheduledBackup {
	b := newBackup(ns, name, repoRef)
	b.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	b.Spec.TimeZone = "America/Chicago"
	last := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	b.Status = borgbasev1.ScheduledBackupStatus{
		EffectiveSchedule:  "17 2 * * *",
		LastSuccessfulTime: &last,
		Conditions: []metav1.Condition{
			{Type: borgbasev1.ScheduledBackupConditionReady, Status: metav1.ConditionTrue, Reason: "Ready"},
		},
	}
	return b
}

func getOutput(t *testing.T, c client.Client, ns string, all bool, output string, args ...string) string {
	t.Helper()
	kinds, err := kindsToList(args)
	if err != nil {
		t.Fatalf("kindsToList: %v", err)
	}
	var buf bytes.Buffer
	if err := runGet(context.Background(), c, &buf, ns, all, output, kinds); err != nil {
		t.Fatalf("runGet: %v", err)
	}
	return buf.String()
}

func TestGetTable(t *testing.T) {
	c := newClient(t, readyRepo("prod", "store"), readyBackup("prod", "web-files", "store"))

	out := getOutput(t, c, "prod", false, OutputTable)

	for _, want := range []string{
		"NAME", "REPO ID", "READY", "INITIALIZED", "USAGE",
		"store", "abcd1234", "True", "2.1 TiB", "4 TiB",
		"REPOSITORY", "SCHEDULE", "LAST BACKUP", "SUSPENDED", "ACTIVE",
		"web-files", "17 2 * * *", "ago", "3h",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
	// The namespace column only appears with -A.
	if strings.Contains(out, "NAMESPACE") {
		t.Errorf("unexpected NAMESPACE column without -A:\n%s", out)
	}
}

func TestGetWideAddsColumns(t *testing.T) {
	c := newClient(t, readyRepo("prod", "store"), readyBackup("prod", "web-files", "store"))

	out := getOutput(t, c, "prod", false, OutputWide)
	for _, want := range []string{
		"SERVER", "abcd1234.repo.borgbase.com", "SECRET", "store-borgbase",
		"TIMEZONE", "America/Chicago",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in wide output:\n%s", want, out)
		}
	}
}

func TestGetAllNamespaces(t *testing.T) {
	c := newClient(t, readyRepo("prod", "store"), readyRepo("staging", "store"))

	out := getOutput(t, c, "", true, OutputTable, "repositories")
	if !strings.Contains(out, "NAMESPACE") {
		t.Errorf("expected NAMESPACE column with -A:\n%s", out)
	}
	for _, ns := range []string{"prod", "staging"} {
		if !strings.Contains(out, ns) {
			t.Errorf("expected namespace %q in output:\n%s", ns, out)
		}
	}
}

func TestGetNarrowsByKind(t *testing.T) {
	c := newClient(t, readyRepo("prod", "store"), readyBackup("prod", "web-files", "store"))

	if out := getOutput(t, c, "prod", false, OutputTable, "repositories"); strings.Contains(out, "web-files") {
		t.Errorf("`get repositories` listed a backup:\n%s", out)
	}
	if out := getOutput(t, c, "prod", false, OutputTable, "backups"); strings.Contains(out, "REPO ID") {
		t.Errorf("`get backups` listed a repository:\n%s", out)
	}
}

func TestGetEmpty(t *testing.T) {
	out := getOutput(t, newClient(t), "prod", false, OutputTable)
	if !strings.Contains(out, `No resources found in namespace "prod".`) {
		t.Errorf("unexpected empty output: %q", out)
	}
	if out := getOutput(t, newClient(t), "", true, OutputTable); !strings.Contains(out, "in any namespace") {
		t.Errorf("unexpected empty -A output: %q", out)
	}
}

func TestGetMachineOutput(t *testing.T) {
	c := newClient(t, readyRepo("prod", "store"))

	jsonOut := getOutput(t, c, "prod", false, OutputJSON, "repositories")
	for _, want := range []string{`"kind": "RepositoryList"`, `"apiVersion"`, `"repositoryID": "abcd1234"`} {
		if !strings.Contains(jsonOut, want) {
			t.Errorf("expected %q in JSON output:\n%s", want, jsonOut)
		}
	}

	yamlOut := getOutput(t, c, "prod", false, OutputYAML, "repositories")
	if !strings.Contains(yamlOut, "kind: RepositoryList") {
		t.Errorf("expected kind in YAML output:\n%s", yamlOut)
	}

	nameOut := getOutput(t, c, "prod", false, OutputName, "repositories")
	if want := "repository.borgbase.clevyr.com/store\n"; nameOut != want {
		t.Errorf("name output = %q, want %q", nameOut, want)
	}
}

func TestGetRejectsBadInput(t *testing.T) {
	if err := ValidateOutput("toml"); err == nil {
		t.Error("expected an error for an unsupported output format")
	}
	if _, err := kindsToList([]string{"cronjobs"}); !errors.Is(err, ErrUnknownKind) {
		t.Errorf("expected ErrUnknownKind, got %v", err)
	}
}

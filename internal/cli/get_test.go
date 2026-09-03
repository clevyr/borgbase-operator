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

func readyRepo(ns string) *borgbasev1.Repository {
	r := newRepo(ns)
	r.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	r.Status = borgbasev1.RepositoryStatus{
		RepositoryID: testRepoID,
		Initialized:  true,
		CurrentUsage: testUsage,
		Quota:        testQuota,
		Server:       testServer,
		SecretName:   testRepoName + "-borgbase",
		Conditions: []metav1.Condition{
			{Type: borgbasev1.RepositoryConditionReady, Status: metav1.ConditionTrue, Reason: reasonReady},
		},
	}
	return r
}

func readyBackup(name, repoRef string) *borgbasev1.ScheduledBackup {
	b := newBackup(name, repoRef)
	b.CreationTimestamp = metav1.NewTime(time.Now().Add(-3 * time.Hour))
	b.Spec.TimeZone = testTimeZone
	last := metav1.NewTime(time.Now().Add(-90 * time.Minute))
	b.Status = borgbasev1.ScheduledBackupStatus{
		EffectiveSchedule:  testSchedule,
		LastSuccessfulTime: &last,
		Conditions: []metav1.Condition{
			{Type: borgbasev1.ScheduledBackupConditionReady, Status: metav1.ConditionTrue, Reason: reasonReady},
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
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	out := getOutput(t, c, "prod", false, OutputTable)

	for _, want := range []string{
		"NAME", "REPO ID", "READY", "INITIALIZED", "USAGE",
		testRepoName, testRepoID, statusTrue, testUsage, testQuota,
		"REPOSITORY", "SCHEDULE", "LAST BACKUP", "SUSPENDED", "ACTIVE",
		testBackupName, testSchedule, "ago", "3h",
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
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	out := getOutput(t, c, "prod", false, OutputWide)
	for _, want := range []string{
		"SERVER", testServer, "SECRET", "store-borgbase",
		"TIMEZONE", testTimeZone,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in wide output:\n%s", want, out)
		}
	}
}

func TestGetAllNamespaces(t *testing.T) {
	c := newClient(t, readyRepo(testNS), readyRepo("staging"))

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
	c := newClient(t, readyRepo(testNS), readyBackup(testBackupName, testRepoName))

	if out := getOutput(t, c, "prod", false, OutputTable, "repositories"); strings.Contains(out, testBackupName) {
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
	c := newClient(t, readyRepo(testNS))

	jsonOut := getOutput(t, c, "prod", false, OutputJSON, "repositories")
	for _, want := range []string{`"kind": "RepositoryList"`, `"apiVersion"`, `"repositoryID": "` + testRepoID + `"`} {
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

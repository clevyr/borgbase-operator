package cli

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"sigs.k8s.io/controller-runtime/pkg/client"
)

func env(t *testing.T, c client.Client, arg string, show bool) (string, string, error) {
	t.Helper()
	var out, errOut bytes.Buffer
	err := runEnv(context.Background(), c, &out, &errOut, "prod", arg, show)
	return out.String(), errOut.String(), err
}

func TestEnvRedactsByDefault(t *testing.T) {
	r := readyRepo(testNS)
	c := newClient(t, r, credentialsSecret(r.SecretName()))

	out, errOut, err := env(t, c, "repo/"+testRepoName, false)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}

	if strings.Contains(out, "pw") && !strings.Contains(out, redacted) {
		t.Errorf("password leaked into output:\n%s", out)
	}
	// The URL embeds the password, so it must be redacted too.
	if strings.Contains(out, "id:pw@") {
		t.Errorf("password leaked via RESTIC_REPOSITORY:\n%s", out)
	}
	for _, want := range []string{"export RESTIC_REPOSITORY=", "export RESTIC_PASSWORD='***'", redacted} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
	// The notice must go to stderr so `eval "$(corg env ...)"` stays clean.
	if !strings.Contains(errOut, "--show-password") {
		t.Errorf("expected a hint on stderr, got %q", errOut)
	}
	if strings.Contains(out, "#") {
		t.Errorf("stdout must contain only export lines:\n%s", out)
	}
}

func TestEnvShowPassword(t *testing.T) {
	r := readyRepo(testNS)
	c := newClient(t, r, credentialsSecret(r.SecretName()))

	out, _, err := env(t, c, "repo/"+testRepoName, true)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	for _, want := range []string{
		"export RESTIC_REPOSITORY='rest:https://id:pw@id.repo.borgbase.com'",
		"export RESTIC_PASSWORD='pw'",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("expected %q in output:\n%s", want, out)
		}
	}
}

// A ScheduledBackup resolves through to its repository's credentials.
func TestEnvViaScheduledBackup(t *testing.T) {
	r := readyRepo(testNS)
	sb := readyBackup(testBackupName, testRepoName)
	c := newClient(t, r, sb, credentialsSecret(r.SecretName()))

	out, _, err := env(t, c, "sb/"+testBackupName, true)
	if err != nil {
		t.Fatalf("runEnv: %v", err)
	}
	if !strings.Contains(out, "id.repo.borgbase.com") {
		t.Errorf("expected the repository URL:\n%s", out)
	}
}

func TestEnvMissingSecret(t *testing.T) {
	c := newClient(t, readyRepo(testNS))
	if _, _, err := env(t, c, "repo/"+testRepoName, false); !errors.Is(err, ErrTargetNotFound) {
		t.Fatalf("expected ErrTargetNotFound, got %v", err)
	}
}

func TestRedactResticURL(t *testing.T) {
	tests := []struct{ in, want string }{
		{"rest:https://abcd:secret@abcd.repo.borgbase.com", "rest:https://abcd:***@abcd.repo.borgbase.com"},
		{"rest:https://abcd@abcd.repo.borgbase.com", "rest:https://abcd@abcd.repo.borgbase.com"},
		// Anything unrecognised is withheld rather than echoed.
		{"s3:s3.amazonaws.com/bucket", redacted},
		{"", redacted},
		{"garbage", redacted},
	}
	for _, tt := range tests {
		if got := RedactResticURL(tt.in); got != tt.want {
			t.Errorf("RedactResticURL(%q) = %q, want %q", tt.in, got, tt.want)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got, want := shellQuote(`a'b`), `'a'\''b'`; got != want {
		t.Errorf("shellQuote = %q, want %q", got, want)
	}
}

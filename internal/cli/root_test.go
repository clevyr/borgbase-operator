package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

func TestDisplayName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{"corg", "corg"},
		{"/usr/local/bin/corg", "corg"},
		{"./bin/corg", "corg"},
		{"kubectl-corg", "kubectl corg"},
		{"/home/u/.krew/bin/kubectl-corg", "kubectl corg"},
		{"kubectl-corg.exe", "kubectl corg"},
		// kubectl accepts an underscore where a plugin name needs a dash.
		{"kubectl-corg_thing", "kubectl corg-thing"},
	}

	for _, tt := range tests {
		if got := DisplayName(tt.argv0); got != tt.want {
			t.Errorf("DisplayName(%q) = %q, want %q", tt.argv0, got, tt.want)
		}
	}
}

func TestRootUsesInvokedIdentity(t *testing.T) {
	for argv0, wantUse := range map[string]string{
		"corg":                  "corg",
		"/usr/bin/kubectl-corg": "corg",
	} {
		cmd := New(testStreams(), argv0)
		if cmd.Use != wantUse {
			t.Errorf("argv0 %q: Use = %q, want %q", argv0, cmd.Use, wantUse)
		}
		if got, want := cmd.Annotations[cobra.CommandDisplayNameAnnotation], DisplayName(argv0); got != want {
			t.Errorf("argv0 %q: display name = %q, want %q", argv0, got, want)
		}
	}
}

// The connection flags must match kubectl's, or muscle memory breaks.
func TestRootRegistersConnectionFlags(t *testing.T) {
	cmd := New(testStreams(), "corg")
	for _, name := range []string{"namespace", "context", "kubeconfig"} {
		if cmd.PersistentFlags().Lookup(name) == nil {
			t.Errorf("missing persistent flag --%s", name)
		}
	}
	if f := cmd.PersistentFlags().ShorthandLookup("n"); f == nil || f.Name != "namespace" {
		t.Error("-n must be shorthand for --namespace")
	}
}

// -A belongs on the commands that can honour it, not on every command.
func TestAllNamespacesIsOptIn(t *testing.T) {
	if f := New(testStreams(), "corg").PersistentFlags().ShorthandLookup("A"); f != nil {
		t.Error("-A must not be a persistent root flag")
	}

	f := NewFactory(testStreams())
	cmd := &cobra.Command{Use: "get"}
	f.AddAllNamespacesFlag(cmd)
	if flag := cmd.Flags().ShorthandLookup("A"); flag == nil || flag.Name != "all-namespaces" {
		t.Error("-A must be shorthand for --all-namespaces")
	}
}

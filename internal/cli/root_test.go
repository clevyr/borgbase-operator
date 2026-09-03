package cli

import (
	"testing"

	"github.com/spf13/cobra"
)

const (
	binaryName    = "corg"
	pluginDisplay = "kubectl " + binaryName
)

func TestDisplayName(t *testing.T) {
	tests := []struct {
		argv0 string
		want  string
	}{
		{binaryName, binaryName},
		{"/usr/local/bin/corg", binaryName},
		{"./bin/corg", binaryName},
		{"kubectl-corg", pluginDisplay},
		{"/opt/homebrew/bin/kubectl-corg", pluginDisplay},
		{"kubectl-corg.exe", pluginDisplay},
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
		binaryName:              binaryName,
		"/usr/bin/kubectl-corg": binaryName,
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
	cmd := New(testStreams(), binaryName)
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
	if f := New(testStreams(), binaryName).PersistentFlags().ShorthandLookup("A"); f != nil {
		t.Error("-A must not be a persistent root flag")
	}

	f := NewFactory(testStreams())
	cmd := &cobra.Command{Use: "get"}
	f.AddAllNamespacesFlag(cmd)
	if flag := cmd.Flags().ShorthandLookup("A"); flag == nil || flag.Name != "all-namespaces" {
		t.Error("-A must be shorthand for --all-namespaces")
	}
}

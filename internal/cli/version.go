package cli

import (
	"fmt"
	"runtime/debug"

	"github.com/spf13/cobra"
)

// version is overridden at release time via -ldflags. When it is empty the
// value baked in by `go build` from VCS metadata is used instead.
var version string

func Version() string {
	if version != "" {
		return version
	}
	if info, ok := debug.ReadBuildInfo(); ok && info.Main.Version != "" {
		return info.Main.Version
	}
	return "unknown"
}

func newVersionCommand(f *Factory) *cobra.Command {
	return &cobra.Command{
		Use:               "version",
		Short:             "Print the corg version",
		Args:              cobra.NoArgs,
		DisableAutoGenTag: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(f.Streams.Out, Version())
			return err
		},
	}
}

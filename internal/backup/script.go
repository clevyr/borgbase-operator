// Package backup renders ScheduledBackup specs into the shell script and
// schedule that drive the generated CronJob.
package backup

import (
	"fmt"
	"strings"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

// Script preamble and postamble, reproduced from the hand-written backups this
// operator replaces so that migrated backups behave identically.
//
// The redirect uses process substitution, which busybox ash supports. The shell
// does not wait for `ts` to drain on exit, so the last line or two of output can
// be lost if the script exits immediately after writing them.
const (
	preamble  = "exec > >(ts '%H:%M:%S') 2>&1\nset -eu"
	postamble = "restic cache --cleanup"
)

// Render returns the full backup script body for a ScheduledBackup.
//
// When spec.Script is set it replaces the generated source and retention lines,
// but still gets the preamble and postamble so that logging, error handling and
// cache cleanup stay uniform across every backup.
func Render(spec *borgbasev1.ScheduledBackupSpec) (string, error) {
	var b strings.Builder
	b.WriteString(preamble)
	b.WriteString("\n")

	if spec.Script != "" {
		b.WriteString(strings.TrimRight(spec.Script, "\n"))
		b.WriteString("\n")
		b.WriteString(postamble)
		b.WriteString("\n")
		return b.String(), nil
	}

	for _, src := range spec.Sources {
		line, err := renderSource(src)
		if err != nil {
			return "", err
		}
		b.WriteString(line)
		b.WriteString("\n")
	}

	if forget := renderForget(spec.Retention); forget != "" {
		b.WriteString(forget)
		b.WriteString("\n")
	}

	b.WriteString(postamble)
	b.WriteString("\n")
	return b.String(), nil
}

// renderSource renders one `restic backup` invocation.
func renderSource(src borgbasev1.BackupSource) (string, error) {
	tag := src.EffectiveTag()

	switch src.Type {
	case borgbasev1.SourceTypeFiles:
		// Files are backed up in place, relative to the container's working
		// directory, with excludes on continuation lines for readability.
		line := fmt.Sprintf("restic backup --tag=%s %s", tag, src.EffectivePath())
		if len(src.Exclude) == 0 {
			return line, nil
		}
		parts := make([]string, 0, len(src.Exclude)+1)
		parts = append(parts, line)
		for _, ex := range src.Exclude {
			parts = append(parts, "  --exclude="+shellQuote(ex))
		}
		return strings.Join(parts, " \\\n"), nil

	case borgbasev1.SourceTypeCNPG, borgbasev1.SourceTypeMariaDB:
		// Database dumps are streamed straight into restic rather than written
		// to disk first, so a dump never needs scratch space the size of the
		// database.
		var sb strings.Builder
		fmt.Fprintf(&sb, "restic backup --tag=%s --stdin-from-command -- dumpdb %s", tag, src.Type)
		if src.Database != "" {
			sb.WriteString(" --database=" + src.Database)
		}
		if len(src.ExtraArgs) > 0 {
			sb.WriteString(" --")
			for _, a := range src.ExtraArgs {
				sb.WriteString(" " + a)
			}
		}
		return sb.String(), nil

	default:
		return "", fmt.Errorf("unknown source type %q", src.Type)
	}
}

// renderForget renders the retention command, or "" when no retention is set.
func renderForget(r *borgbasev1.Retention) string {
	if r == nil {
		return ""
	}
	flags := make([]string, 0, 6)
	for _, f := range []struct {
		name  string
		value *int32
	}{
		{"last", r.Last},
		{"hourly", r.Hourly},
		{"daily", r.Daily},
		{"weekly", r.Weekly},
		{"monthly", r.Monthly},
		{"yearly", r.Yearly},
	} {
		if f.value != nil {
			flags = append(flags, fmt.Sprintf("--keep-%s=%d", f.name, *f.value))
		}
	}
	if len(flags) == 0 {
		return ""
	}
	return "restic forget --prune " + strings.Join(flags, " ")
}

// shellQuote wraps s in single quotes so that glob patterns reach restic
// instead of being expanded by the shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

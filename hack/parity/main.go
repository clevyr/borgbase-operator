// Command parity compares the backup script this operator would render against
// the script in the hand-written HelmRelease it replaces.
//
// Migrating an app must not silently change what gets backed up, so run this
// for each app before cutting it over and account for every difference.
//
// Usage:
//
//	go run ./hack/parity <generated.yaml> <helmrelease.yaml>
package main

import (
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"sigs.k8s.io/yaml"
)

func main() {
	if len(os.Args) != 3 {
		fmt.Fprintln(os.Stderr, "usage: parity <generated.yaml> <helmrelease.yaml>")
		os.Exit(2)
	}

	rendered, err := renderGenerated(os.Args[1])
	if err != nil {
		fmt.Fprintln(os.Stderr, "rendering generated resource:", err)
		os.Exit(1)
	}
	original, err := originalScript(os.Args[2])
	if err != nil {
		fmt.Fprintln(os.Stderr, "reading original script:", err)
		os.Exit(1)
	}

	if normalize(rendered) == normalize(original) {
		fmt.Println("IDENTICAL")
		return
	}

	fmt.Println("--- original")
	fmt.Println(original)
	fmt.Println("--- rendered")
	fmt.Println(rendered)
	os.Exit(1)
}

func renderGenerated(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	for doc := range strings.SplitSeq(string(raw), "\n---\n") {
		if !strings.Contains(doc, "kind: ScheduledBackup") {
			continue
		}
		var sb borgbasev1.ScheduledBackup
		if err := yaml.Unmarshal([]byte(doc), &sb); err != nil {
			return "", err
		}
		return backup.Render(&sb.Spec)
	}
	return "", fmt.Errorf("no ScheduledBackup document in %s", path)
}

// originalScript pulls the shell body out of the HelmRelease's container
// command, which is the last element of the runitor invocation.
func originalScript(path string) (string, error) {
	out, err := exec.Command("yq", "-r",
		".spec.values.controllers.restic.containers.restic.command[-1]", path).Output()
	if err != nil {
		return "", fmt.Errorf("running yq: %w", err)
	}
	return string(out), nil
}

// normalize ignores differences that cannot change what gets backed up:
// trailing whitespace, the optional quoting around an --exclude pattern, and
// the --retry-lock flag.
//
// Quoting a bare pattern is a safety improvement, not a change in what it
// matches. --retry-lock only decides whether a command waits for the repository
// lock or fails immediately; it selects no different data. Both are deliberate
// improvements over the hand-written scripts, so comparing them verbatim would
// report a difference on every app and hide the ones that matter.
var retryLock = regexp.MustCompile(` --retry-lock=\S+`)

func normalize(s string) string {
	var lines []string
	for line := range strings.SplitSeq(s, "\n") {
		line = strings.TrimRight(line, " \t\\")
		line = retryLock.ReplaceAllString(line, "")
		// Only unquote exclude patterns; stripping a trailing quote from every
		// line could hide a genuine difference elsewhere in the script.
		if strings.Contains(line, "--exclude=") {
			line = strings.ReplaceAll(line, "--exclude='", "--exclude=")
			line = strings.TrimSuffix(strings.TrimRight(line, " \t"), "'")
		}
		if trimmed := strings.TrimSpace(line); trimmed != "" {
			lines = append(lines, trimmed)
		}
	}
	return strings.Join(lines, "\n")
}

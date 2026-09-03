package cli

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"slices"
	"strconv"
	"strings"

	"sigs.k8s.io/yaml"
)

var (
	ErrNoResticController = errors.New("no restic controller in the HelmRelease")
	ErrNoResticScript     = errors.New("no restic command in the HelmRelease")
)

// resticController is the key the hand-written HelmReleases use for the backup
// controller and its container.
const resticController = "restic"

// helmRelease is the subset of a bjw-s app-template HelmRelease that describes
// a backup. Parsing it into types rather than querying it with yq means a
// missing field is a compile-time shape, an unexpected one is an error, and
// nothing depends on an external tool being installed.
type helmRelease struct {
	Metadata struct {
		Namespace string `json:"namespace"`
	} `json:"metadata"`
	Spec struct {
		Values struct {
			Controllers map[string]hrController  `json:"controllers"`
			Persistence map[string]hrPersistence `json:"persistence"`
		} `json:"values"`
	} `json:"spec"`
}

type hrController struct {
	CronJob struct {
		Schedule          string `json:"schedule"`
		ConcurrencyPolicy string `json:"concurrencyPolicy"`
		TimeZone          string `json:"timeZone"`
	} `json:"cronjob"`
	Containers map[string]hrContainer `json:"containers"`
}

type hrContainer struct {
	Command    []string `json:"command"`
	WorkingDir string   `json:"workingDir"`
	// Env values are usually plain strings but may be a valueFrom object, so
	// they are read loosely and coerced.
	Env map[string]any `json:"env"`
}

type hrPersistence struct {
	Type          string `json:"type"`
	Name          string `json:"name"`
	ExistingClaim string `json:"existingClaim"`
	StorageClass  string `json:"storageClass"`
}

func readHelmRelease(path string) (*helmRelease, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var hr helmRelease
	if err := yaml.Unmarshal(raw, &hr); err != nil {
		return nil, fmt.Errorf("parsing %s: %w", path, err)
	}
	return &hr, nil
}

// backupController returns the restic controller and its container.
func (hr *helmRelease) backupController() (hrController, hrContainer, error) {
	controller, ok := hr.Spec.Values.Controllers[resticController]
	if !ok {
		return hrController{}, hrContainer{}, ErrNoResticController
	}
	container, ok := controller.Containers[resticController]
	if !ok {
		return hrController{}, hrContainer{}, ErrNoResticScript
	}
	return controller, container, nil
}

// script is the shell body, which is the last element of the runitor
// invocation.
func (c hrContainer) script() (string, error) {
	if len(c.Command) == 0 {
		return "", ErrNoResticScript
	}
	return c.Command[len(c.Command)-1], nil
}

func (c hrContainer) env(name string) string {
	v, ok := c.Env[name]
	if !ok {
		return ""
	}
	switch value := v.(type) {
	case string:
		return value
	case int, int64, float64, bool:
		return fmt.Sprint(value)
	default:
		// A valueFrom reference cannot be carried across as a literal.
		return ""
	}
}

// existingClaim returns the first persistence entry backed by a claim, which is
// the volume being backed up.
func (hr *helmRelease) existingClaim() string {
	for _, name := range sortedKeys(hr.Spec.Values.Persistence) {
		if claim := hr.Spec.Values.Persistence[name].ExistingClaim; claim != "" {
			return claim
		}
	}
	return ""
}

// databaseSecret returns the first Secret-typed persistence entry, which is
// where the database credentials are mounted.
func (hr *helmRelease) databaseSecret() string {
	for _, name := range sortedKeys(hr.Spec.Values.Persistence) {
		p := hr.Spec.Values.Persistence[name]
		if p.Type == "secret" && p.Name != "" {
			return p.Name
		}
	}
	return ""
}

func (hr *helmRelease) cacheStorageClass() string {
	return hr.Spec.Values.Persistence["cache"].StorageClass
}

// splitArgs splits a shell line into words, honouring single and double quotes.
//
// The hand-written scripts quote exclude patterns inconsistently -- some are
// single-quoted, some bare -- and a naive split on whitespace would break a
// quoted pattern containing a space into two excludes, silently widening what
// is backed up.
func splitArgs(line string) []string {
	var args []string
	var current strings.Builder
	var quote rune
	inWord := false

	for _, r := range line {
		switch {
		case quote != 0:
			if r == quote {
				quote = 0
			} else {
				current.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			inWord = true
		case r == ' ' || r == '\t':
			if inWord {
				args = append(args, current.String())
				current.Reset()
				inWord = false
			}
		default:
			current.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		args = append(args, current.String())
	}
	return args
}

// flagValue returns the value of --name=value, and whether it was present.
func flagValue(args []string, name string) (string, bool) {
	prefix := "--" + name + "="
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			return v, true
		}
	}
	return "", false
}

// flagValues returns every occurrence of --name=value.
func flagValues(args []string, name string) []string {
	prefix := "--" + name + "="
	var out []string
	for _, a := range args {
		if v, ok := strings.CutPrefix(a, prefix); ok {
			out = append(out, v)
		}
	}
	return out
}

func parseInt32(s string) (*int32, error) {
	n, err := strconv.ParseInt(s, 10, 32)
	if err != nil {
		return nil, err
	}
	v := int32(n)
	return &v, nil
}

// joinContinuations folds a script's backslash line continuations, so one
// logical restic invocation is one line.
func joinContinuations(script string) []string {
	var lines []string
	var current strings.Builder

	for line := range strings.SplitSeq(script, "\n") {
		trimmed := strings.TrimSpace(line)
		if after, ok := strings.CutSuffix(trimmed, "\\"); ok {
			current.WriteString(strings.TrimSpace(after))
			current.WriteString(" ")
			continue
		}
		current.WriteString(trimmed)
		if joined := strings.TrimSpace(current.String()); joined != "" {
			lines = append(lines, joined)
		}
		current.Reset()
	}
	if joined := strings.TrimSpace(current.String()); joined != "" {
		lines = append(lines, joined)
	}
	return lines
}

// sortedKeys makes map iteration deterministic, so the same HelmRelease always
// migrates to the same resource.
func sortedKeys[V any](m map[string]V) []string {
	return slices.Sorted(maps.Keys(m))
}

// scheduleOf returns the schedule as written in the HelmRelease.
func (hr *helmRelease) scheduleOf() string {
	return hr.Spec.Values.Controllers[resticController].CronJob.Schedule
}

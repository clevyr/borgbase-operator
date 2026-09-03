package cli

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

type TargetKind string

const (
	TargetRepository      TargetKind = "repository"
	TargetScheduledBackup TargetKind = "scheduledbackup"
)

var (
	ErrTargetNotFound = errors.New("not found")
	ErrAmbiguous      = errors.New("ambiguous name")
	ErrUnknownKind    = errors.New("unknown resource type")
)

// kindAliases maps every accepted prefix to its kind. These mirror the short
// names the CRDs are registered under, plus the obvious abbreviations.
var kindAliases = map[string]TargetKind{
	"repository":       TargetRepository,
	"repositories":     TargetRepository,
	"repo":             TargetRepository,
	"repos":            TargetRepository,
	"scheduledbackup":  TargetScheduledBackup,
	"scheduledbackups": TargetScheduledBackup,
	"sb":               TargetScheduledBackup,
	"backup":           TargetScheduledBackup,
	"backups":          TargetScheduledBackup,
}

// Target is a resolved command argument.
type Target struct {
	Kind            TargetKind
	Repository      *borgbasev1.Repository
	ScheduledBackup *borgbasev1.ScheduledBackup
}

func (t *Target) Name() string {
	if t.Kind == TargetRepository {
		return t.Repository.Name
	}
	return t.ScheduledBackup.Name
}

func (t *Target) Namespace() string {
	if t.Kind == TargetRepository {
		return t.Repository.Namespace
	}
	return t.ScheduledBackup.Namespace
}

// Resolve turns a command argument into the object it names.
//
// It accepts a bare name, or one qualified by kind: "web-files", "sb/web-files"
// and "repository/prod" are all valid. A bare name that matches both kinds is
// rejected rather than guessed.
func Resolve(ctx context.Context, c client.Client, namespace, arg string) (*Target, error) {
	if arg == "" {
		return nil, fmt.Errorf("%w: empty name", ErrTargetNotFound)
	}

	if prefix, name, ok := strings.Cut(arg, "/"); ok {
		kind, known := kindAliases[strings.ToLower(prefix)]
		if !known {
			return nil, fmt.Errorf("%w %q, expected one of: %s",
				ErrUnknownKind, prefix, strings.Join(knownPrefixes(), ", "))
		}
		if name == "" {
			return nil, fmt.Errorf("%w: %q has no name", ErrTargetNotFound, arg)
		}
		return get(ctx, c, namespace, kind, name)
	}

	sb, sbErr := getScheduledBackup(ctx, c, namespace, arg)
	repo, repoErr := getRepository(ctx, c, namespace, arg)

	switch {
	case sbErr == nil && repoErr == nil:
		return nil, fmt.Errorf(
			"%w: %q is both a ScheduledBackup and a Repository; qualify it as sb/%s or repo/%s",
			ErrAmbiguous, arg, arg, arg)
	case sbErr == nil:
		return &Target{Kind: TargetScheduledBackup, ScheduledBackup: sb}, nil
	case repoErr == nil:
		return &Target{Kind: TargetRepository, Repository: repo}, nil
	}

	// Surface a real failure (RBAC, connectivity) rather than reporting it as
	// a missing object.
	for _, err := range []error{sbErr, repoErr} {
		if !apierrors.IsNotFound(err) {
			return nil, err
		}
	}
	return nil, fmt.Errorf("%w: no ScheduledBackup or Repository named %q in namespace %q",
		ErrTargetNotFound, arg, namespace)
}

func get(ctx context.Context, c client.Client, namespace string, kind TargetKind, name string) (*Target, error) {
	if kind == TargetRepository {
		repo, err := getRepository(ctx, c, namespace, name)
		if err != nil {
			return nil, wrapNotFound(err, "Repository", namespace, name)
		}
		return &Target{Kind: kind, Repository: repo}, nil
	}

	sb, err := getScheduledBackup(ctx, c, namespace, name)
	if err != nil {
		return nil, wrapNotFound(err, "ScheduledBackup", namespace, name)
	}
	return &Target{Kind: kind, ScheduledBackup: sb}, nil
}

func getRepository(ctx context.Context, c client.Client, namespace, name string) (*borgbasev1.Repository, error) {
	var repo borgbasev1.Repository
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &repo); err != nil {
		return nil, err
	}
	return &repo, nil
}

func getScheduledBackup(ctx context.Context, c client.Client, namespace, name string) (*borgbasev1.ScheduledBackup, error) {
	var sb borgbasev1.ScheduledBackup
	if err := c.Get(ctx, types.NamespacedName{Namespace: namespace, Name: name}, &sb); err != nil {
		return nil, err
	}
	return &sb, nil
}

// RepositoryFor loads the Repository a ScheduledBackup points at. Backups are
// namespace-scoped and so is the reference.
func RepositoryFor(ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup) (*borgbasev1.Repository, error) {
	name := sb.Spec.RepositoryRef.Name
	repo, err := getRepository(ctx, c, sb.Namespace, name)
	if err != nil {
		return nil, wrapNotFound(err, "Repository", sb.Namespace,
			fmt.Sprintf("%s (referenced by scheduledbackup/%s)", name, sb.Name))
	}
	return repo, nil
}

func wrapNotFound(err error, kind, namespace, name string) error {
	if apierrors.IsNotFound(err) {
		return fmt.Errorf("%w: no %s named %q in namespace %q", ErrTargetNotFound, kind, name, namespace)
	}
	return err
}

func knownPrefixes() []string {
	out := make([]string, 0, len(kindAliases))
	for k := range kindAliases {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

package cli

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/util/validation"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func TestNewKeySecretNameIsUniquePerCall(t *testing.T) {
	repo := &borgbasev1.Repository{Name: "myapp", Namespace: "acme-prod"}
	prefix := repo.Name + "-corg-newkey-"

	seen := make(map[string]struct{}, 100)
	for range 100 {
		name := newKeySecretName(repo)

		if !strings.HasPrefix(name, prefix) {
			t.Fatalf("newKeySecretName() = %q, want the prefix %q", name, prefix)
		}
		if suffix := strings.TrimPrefix(name, prefix); len(suffix) != 5 {
			t.Fatalf("newKeySecretName() = %q, want a five character suffix, got %q", name, suffix)
		}
		if errs := validation.IsDNS1123Subdomain(name); len(errs) > 0 {
			t.Fatalf("newKeySecretName() = %q is not a valid Secret name: %v", name, errs)
		}
		if _, ok := seen[name]; ok {
			t.Fatalf("newKeySecretName() returned %q twice; a reused name lets a stale "+
				"Secret supply the password that restic key add reads", name)
		}
		seen[name] = struct{}{}
	}
}

package cli

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"sigs.k8s.io/yaml"
)

var ErrSopsUnavailable = errors.New("sops is required to read the encrypted repository ID")

// decryptedSecret is the shape of the app's secret.yaml once sops has opened it.
type decryptedSecret struct {
	StringData map[string]string `json:"stringData"`
	Data       map[string]string `json:"data"`
}

// repositoryIDFromSecret recovers the BorgBase repository ID, which is recorded
// nowhere except inside the encrypted RESTIC_REPOSITORY value.
//
// Decryption is delegated to the sops binary rather than linked in. The sops
// library pulls every KMS backend it supports -- AWS, Azure, GCP, Vault, age
// and PGP -- which measured at 61MB on top of a 93MB binary, for one field read
// once per app during a migration that happens once. Decryption is also the one
// step that needs the operator's own cloud credentials, which is exactly what
// the sops binary already manages. Everything else about this migration is
// parsed and emitted in Go, and --repository-id skips this path entirely.
func repositoryIDFromSecret(path string) (string, error) {
	if _, err := os.Stat(path); err != nil {
		return "", fmt.Errorf("no secret.yaml alongside the HelmRelease: %w; "+
			"pass --repository-id to supply the id directly", err)
	}
	if _, err := exec.LookPath("sops"); err != nil {
		return "", fmt.Errorf("%w: install sops, or pass --repository-id", ErrSopsUnavailable)
	}

	plaintext, err := exec.Command("sops", "--decrypt", path).Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && len(exitErr.Stderr) > 0 {
			return "", fmt.Errorf("decrypting %s: %w: %s",
				path, err, strings.TrimSpace(string(exitErr.Stderr)))
		}
		return "", fmt.Errorf("decrypting %s: %w", path, err)
	}

	var secret decryptedSecret
	if err := yaml.Unmarshal(plaintext, &secret); err != nil {
		return "", fmt.Errorf("parsing the decrypted %s: %w", path, err)
	}

	url := secret.StringData["RESTIC_REPOSITORY"]
	if url == "" {
		url = secret.Data["RESTIC_REPOSITORY"]
	}
	if url == "" {
		return "", fmt.Errorf("%w: %s has no RESTIC_REPOSITORY", ErrNoRepositoryID, path)
	}
	return repositoryIDFromURL(url)
}

// repositoryIDFromURL pulls the id out of rest:https://<id>:<password>@<host>.
func repositoryIDFromURL(url string) (string, error) {
	rest, ok := strings.CutPrefix(url, "rest:https://")
	if !ok {
		return "", fmt.Errorf("%w: %q is not a rest: repository URL", ErrNoRepositoryID, url)
	}
	id, _, ok := strings.Cut(rest, ":")
	if !ok || id == "" {
		return "", fmt.Errorf("%w: no id in %q", ErrNoRepositoryID, url)
	}
	return id, nil
}

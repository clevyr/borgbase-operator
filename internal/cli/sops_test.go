package cli

import (
	"errors"
	"testing"

	"sigs.k8s.io/yaml"
)

func TestRepositoryIDFromURL(t *testing.T) {
	tests := []struct {
		name    string
		url     string
		want    string
		wantErr bool
	}{
		{"rest url", "rest:https://ab12cd34:s3cret@ab12cd34.repo.borgbase.com/./", "ab12cd34", false},
		{"no password", "rest:https://ab12cd34@ab12cd34.repo.borgbase.com/./", "", true},
		{"not a rest url", "s3:s3.amazonaws.com/bucket", "", true},
		{"plain https", "https://ab12cd34:s3cret@ab12cd34.repo.borgbase.com/./", "", true},
		{"empty id", "rest:https://:s3cret@ab12cd34.repo.borgbase.com/./", "", true},
		{"empty", "", "", true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			id, err := repositoryIDFromURL(tt.url)
			if tt.wantErr {
				if err == nil {
					t.Fatalf("repositoryIDFromURL(%q) = %q, want an error", tt.url, id)
				}
				if !errors.Is(err, ErrNoRepositoryID) {
					t.Errorf("error = %v, want it to wrap ErrNoRepositoryID", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("repositoryIDFromURL(%q) error = %v", tt.url, err)
			}
			if id != tt.want {
				t.Errorf("repositoryIDFromURL(%q) = %q, want %q", tt.url, id, tt.want)
			}
		})
	}
}

func TestDecryptedSecretReadsBothShapes(t *testing.T) {
	const url = "rest:https://ab12cd34:s3cret@ab12cd34.repo.borgbase.com/./"

	tests := []struct {
		name      string
		plaintext string
	}{
		{"stringData", `
apiVersion: v1
kind: Secret
metadata:
  name: restic-envs
stringData:
  RESTIC_REPOSITORY: ` + url + `
  RESTIC_PASSWORD: hunter2
`},
		{"base64 data", `
apiVersion: v1
kind: Secret
metadata:
  name: restic-envs
data:
  RESTIC_REPOSITORY: cmVzdDpodHRwczovL2FiMTJjZDM0OnMzY3JldEBhYjEyY2QzNC5yZXBvLmJvcmdiYXNlLmNvbS8uLw==
  RESTIC_PASSWORD: aHVudGVyMg==
`},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var secret decryptedSecret
			if err := yaml.Unmarshal([]byte(tt.plaintext), &secret); err != nil {
				t.Fatalf("unmarshalling the decrypted secret: %v", err)
			}

			got := secret.StringData["RESTIC_REPOSITORY"]
			if got == "" {
				got = string(secret.Data["RESTIC_REPOSITORY"])
			}
			if got != url {
				t.Fatalf("RESTIC_REPOSITORY = %q, want %q", got, url)
			}

			id, err := repositoryIDFromURL(got)
			if err != nil {
				t.Fatalf("repositoryIDFromURL error = %v", err)
			}
			if id != "ab12cd34" {
				t.Errorf("id = %q, want %q", id, "ab12cd34")
			}
		})
	}
}

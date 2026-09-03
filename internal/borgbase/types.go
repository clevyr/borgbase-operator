// Package borgbase is a minimal client for the BorgBase GraphQL API at
// https://api.borgbase.com/graphql.
//
// Only the repository operations this operator needs are implemented. The
// schema was taken from the API's own introspection endpoint, which is
// reachable without authentication.
package borgbase

import "errors"

// FormatRestic is the only repository format this operator manages. BorgBase
// also serves borg repositories, but they are explicitly out of scope.
//
// repoAdd accepts `format` while repoEdit does not: a repository's format is
// fixed at creation.
const FormatRestic = "restic"

// Errors returned by the client.
var (
	// ErrNotFound means the repository does not exist, or the token cannot see it.
	ErrNotFound = errors.New("borgbase: repository not found")

	// ErrNotRestic means a repository exists but is not restic-format. Since
	// format cannot be changed after creation, this is terminal.
	ErrNotRestic = errors.New("borgbase: repository is not restic format")

	// ErrNoCredentials means the API returned a repository with no usable REST
	// password, so no RESTIC_REPOSITORY URL can be built for it.
	ErrNoCredentials = errors.New("borgbase: repository has no REST credentials")
)

// Repo mirrors the fields of BorgBase's RepoType that this operator reads.
type Repo struct {
	ID           string  `json:"id"`
	Name         string  `json:"name"`
	Region       string  `json:"region"`
	Format       string  `json:"format"`
	RepoPath     string  `json:"repoPath"`
	Quota        int64   `json:"quota"`
	QuotaEnabled bool    `json:"quotaEnabled"`
	AlertDays    int64   `json:"alertDays"`
	AppendOnly   bool    `json:"appendOnly"`
	CurrentUsage float64 `json:"currentUsage"`

	// VgerToken and Htpasswd carry the REST-server credentials. BorgBase
	// populates one or the other depending on the repository; Password picks
	// whichever is present.
	VgerToken string `json:"vgerToken"`
	Htpasswd  string `json:"htpasswd"`

	Server Server `json:"server"`
}

// Server is BorgBase's ServerType.
type Server struct {
	Hostname string `json:"hostname"`
	Region   string `json:"region"`
}

// IsRestic reports whether this is a restic-format repository.
func (r *Repo) IsRestic() bool { return r.Format == FormatRestic }

// Password returns the REST-server password used in the repository URL.
func (r *Repo) Password() string {
	if r.VgerToken != "" {
		return r.VgerToken
	}
	return r.Htpasswd
}

// Host returns the hostname serving this repository. BorgBase serves each repo
// from a subdomain named after the repo ID; Server.Hostname is preferred when
// the API reports one.
func (r *Repo) Host() string {
	if r.Server.Hostname != "" {
		return r.Server.Hostname
	}
	return r.ID + ".repo.borgbase.com"
}

// ResticURL builds the RESTIC_REPOSITORY value for this repository, in the
// form rest:https://<id>:<password>@<host>.
//
// The HTTP username is the repository ID, which is also its subdomain. Note
// that the returned string contains a credential.
func (r *Repo) ResticURL() (string, error) {
	if !r.IsRestic() {
		return "", ErrNotRestic
	}
	pass := r.Password()
	if pass == "" {
		return "", ErrNoCredentials
	}
	return "rest:https://" + r.ID + ":" + pass + "@" + r.Host(), nil
}

// AddOptions are the arguments to repoAdd.
type AddOptions struct {
	Name       string
	Region     string
	Quota      *int64
	AlertDays  *int64
	AppendOnly bool
}

// EditOptions are the arguments to repoEdit. Note the absence of Format.
type EditOptions struct {
	Quota      *int64
	AlertDays  *int64
	AppendOnly bool
}

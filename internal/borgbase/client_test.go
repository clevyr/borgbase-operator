package borgbase

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// stub serves canned GraphQL responses and records the requests it received.
type stub struct {
	response string
	requests []map[string]any
	authz    string
}

func (s *stub) server(t *testing.T) (*Client, func()) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		s.authz = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		var req map[string]any
		_ = json.Unmarshal(body, &req)
		s.requests = append(s.requests, req)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, s.response)
	}))
	c := NewClient("test-token")
	c.Endpoint = srv.URL
	return c, srv.Close
}

func (s *stub) lastQuery() string {
	if len(s.requests) == 0 {
		return ""
	}
	q, _ := s.requests[len(s.requests)-1]["query"].(string)
	return q
}

func (s *stub) lastVars() map[string]any {
	if len(s.requests) == 0 {
		return nil
	}
	v, _ := s.requests[len(s.requests)-1]["variables"].(map[string]any)
	return v
}

func TestGetSendsBearerTokenAndParsesRepo(t *testing.T) {
	s := &stub{response: `{"data":{"repo":{
		"id":"a1b2c3d4","name":"myapp-prod","region":"us","format":"restic",
		"repoPath":"a1b2c3d4@a1b2c3d4.repo.borgbase.com:repo",
		"quota":100,"quotaEnabled":true,"alertDays":3,"appendOnly":false,
		"currentUsage":3.25,"vgerToken":"vger-secret","htpasswd":null,
		"server":{"hostname":"a1b2c3d4.repo.borgbase.com","region":"us"}}}}`}
	c, done := s.server(t)
	defer done()

	repo, err := c.Get(context.Background(), "a1b2c3d4")
	if err != nil {
		t.Fatalf("Get() error = %v", err)
	}
	if s.authz != "Bearer test-token" {
		t.Errorf("Authorization = %q", s.authz)
	}
	if repo.ID != "a1b2c3d4" || !repo.IsRestic() || repo.CurrentUsage != 3.25 {
		t.Errorf("Get() = %+v", repo)
	}

	url, err := repo.ResticURL()
	if err != nil {
		t.Fatalf("ResticURL() error = %v", err)
	}
	want := "rest:https://a1b2c3d4:vger-secret@a1b2c3d4.repo.borgbase.com"
	if url != want {
		t.Errorf("ResticURL() = %q, want %q", url, want)
	}
}

// BorgBase reports a missing repository as a GraphQL error, which must surface
// as ErrNotFound so the controller can tell "absent" from "broken".
func TestGetMapsNotFound(t *testing.T) {
	s := &stub{response: `{"errors":[{"message":"Repository not found"}]}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

func TestGetNullRepoIsNotFound(t *testing.T) {
	s := &stub{response: `{"data":{"repo":null}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Get(context.Background(), "missing"); !errors.Is(err, ErrNotFound) {
		t.Errorf("Get() error = %v, want ErrNotFound", err)
	}
}

// Every repository this operator creates must be restic-format, since the
// format cannot be changed afterwards.
func TestAddAlwaysRequestsResticFormat(t *testing.T) {
	s := &stub{response: `{"data":{"repoAdd":{"repoAdded":{
		"id":"new12345","name":"myapp-prod","region":"us","format":"restic",
		"vgerToken":"t","server":{"hostname":"new12345.repo.borgbase.com"}}}}}`}
	c, done := s.server(t)
	defer done()

	quota := int64(100)
	if _, err := c.Add(context.Background(), AddOptions{
		Name: "myapp-prod", Region: "us", Quota: &quota,
	}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}

	vars := s.lastVars()
	if vars["format"] != FormatRestic {
		t.Errorf("format = %v, want %q", vars["format"], FormatRestic)
	}
	if vars["quotaEnabled"] != true {
		t.Errorf("quotaEnabled = %v, want true when a quota is set", vars["quotaEnabled"])
	}
}

func TestAddOmitsQuotaWhenUnset(t *testing.T) {
	s := &stub{response: `{"data":{"repoAdd":{"repoAdded":{
		"id":"new12345","format":"restic","vgerToken":"t","server":{"hostname":"h"}}}}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Add(context.Background(), AddOptions{Name: "n", Region: "us"}); err != nil {
		t.Fatalf("Add() error = %v", err)
	}
	vars := s.lastVars()
	if _, present := vars["quota"]; present {
		t.Error("quota must be omitted entirely when unset")
	}
	if vars["quotaEnabled"] != false {
		t.Errorf("quotaEnabled = %v, want false", vars["quotaEnabled"])
	}
}

// repoEdit has no format argument, so sending one would be an API error.
func TestEditNeverSendsFormat(t *testing.T) {
	s := &stub{response: `{"data":{"repoEdit":{"repoEdited":{
		"id":"a1b2c3d4","format":"restic","vgerToken":"t","server":{"hostname":"h"}}}}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.Edit(context.Background(), "a1b2c3d4", EditOptions{}); err != nil {
		t.Fatalf("Edit() error = %v", err)
	}
	// `format` legitimately appears in the response selection set; what must
	// not appear is a $format argument, which repoEdit does not accept.
	if strings.Contains(s.lastQuery(), "$format") {
		t.Error("repoEdit must not pass format; it is fixed at creation")
	}
	if _, present := s.lastVars()["format"]; present {
		t.Error("repoEdit must not send a format variable")
	}
}

// Two repositories sharing a name is ambiguous. Picking one could back up into
// the wrong place, so the client refuses instead of guessing.
func TestFindByNameRefusesAmbiguousMatches(t *testing.T) {
	s := &stub{response: `{"data":{"repoList":[
		{"id":"aaa","name":"myapp-prod","format":"restic"},
		{"id":"bbb","name":"myapp-prod","format":"restic"}]}}`}
	c, done := s.server(t)
	defer done()

	_, err := c.FindByName(context.Background(), "myapp-prod")
	if err == nil || errors.Is(err, ErrNotFound) {
		t.Fatalf("FindByName() error = %v, want an ambiguity error", err)
	}
}

// repoList's name argument is a server-side filter of undocumented semantics,
// so near-misses must be discarded rather than adopted.
func TestFindByNameRequiresExactMatch(t *testing.T) {
	s := &stub{response: `{"data":{"repoList":[{"id":"aaa","name":"myapp-prod-old","format":"restic"}]}}`}
	c, done := s.server(t)
	defer done()

	if _, err := c.FindByName(context.Background(), "myapp-prod"); !errors.Is(err, ErrNotFound) {
		t.Errorf("FindByName() error = %v, want ErrNotFound", err)
	}
}

func TestResticURLRejectsUnusableRepos(t *testing.T) {
	if _, err := (&Repo{ID: "x", Format: "borg"}).ResticURL(); !errors.Is(err, ErrNotRestic) {
		t.Error("a borg repository must not yield a restic URL")
	}
	if _, err := (&Repo{ID: "x", Format: FormatRestic}).ResticURL(); !errors.Is(err, ErrNoCredentials) {
		t.Error("a repository with no REST password must not yield a URL")
	}
}

func TestPasswordPrefersVgerTokenThenHtpasswd(t *testing.T) {
	if got := (&Repo{VgerToken: "v", Htpasswd: "h"}).Password(); got != "v" {
		t.Errorf("Password() = %q, want the vger token", got)
	}
	if got := (&Repo{Htpasswd: "h"}).Password(); got != "h" {
		t.Errorf("Password() = %q, want the htpasswd fallback", got)
	}
}

func TestHostFallsBackToRepoSubdomain(t *testing.T) {
	if got := (&Repo{ID: "abc123"}).Host(); got != "abc123.repo.borgbase.com" {
		t.Errorf("Host() = %q", got)
	}
}

func TestNonOKStatusIsAnError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer srv.Close()
	c := NewClient("bad")
	c.Endpoint = srv.URL

	if _, err := c.Get(context.Background(), "x"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

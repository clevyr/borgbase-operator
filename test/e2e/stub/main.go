package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"strings"
	"sync"
)

type repo struct {
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
	Htpasswd     string  `json:"htpasswd"`
	Server       struct {
		Hostname string `json:"hostname"`
		Region   string `json:"region"`
	} `json:"server"`
}

type store struct {
	mu    sync.Mutex
	repos map[string]*repo
	next  int
}

type request struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
}

func main() {
	s := &store{repos: map[string]*repo{}}
	http.HandleFunc("/graphql", s.handle)
	http.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})

	addr := os.Getenv("ADDR")
	if addr == "" {
		addr = ":8080"
	}
	log.Printf("borgbase stub listening on %s", addr)
	server := &http.Server{Addr: addr}
	if err := server.ListenAndServe(); err != nil {
		log.Fatal(err)
	}
}

func (s *store) handle(w http.ResponseWriter, r *http.Request) {
	var req request
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, "malformed request")
		return
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	switch {
	case strings.Contains(req.Query, "query repoList"):
		name, _ := req.Variables["name"].(string)
		var found []*repo
		for _, r := range s.repos {
			if r.Name == name {
				found = append(found, r)
			}
		}
		writeData(w, map[string]any{"repoList": found})

	case strings.Contains(req.Query, "query repo("):
		id, _ := req.Variables["id"].(string)
		r, ok := s.repos[id]
		if !ok {
			writeErr(w, "Repository not found")
			return
		}
		writeData(w, map[string]any{"repo": r})

	case strings.Contains(req.Query, "mutation repoAdd"):
		s.next++
		id := fmt.Sprintf("stub%04d", s.next)
		r := &repo{
			ID:       id,
			Name:     str(req.Variables["name"]),
			Region:   str(req.Variables["region"]),
			Format:   str(req.Variables["format"]),
			Htpasswd: "stub-password-" + id,
		}
		r.Server.Hostname = id + ".repo.borgbase.com"
		apply(r, req.Variables)
		s.repos[id] = r
		writeData(w, map[string]any{"repoAdd": map[string]any{"repoAdded": r}})

	case strings.Contains(req.Query, "mutation repoEdit"):
		id := str(req.Variables["id"])
		r, ok := s.repos[id]
		if !ok {
			writeErr(w, "Repository not found")
			return
		}
		apply(r, req.Variables)
		writeData(w, map[string]any{"repoEdit": map[string]any{"repoEdited": r}})

	case strings.Contains(req.Query, "mutation repoDelete"):
		delete(s.repos, str(req.Variables["id"]))
		writeData(w, map[string]any{"repoDelete": map[string]any{"ok": true}})

	default:
		writeErr(w, "unsupported operation")
	}
}

func apply(r *repo, vars map[string]any) {
	if v, ok := vars["quota"].(float64); ok {
		r.Quota = int64(v)
	}
	if v, ok := vars["quotaEnabled"].(bool); ok {
		r.QuotaEnabled = v
	}
	if v, ok := vars["alertDays"].(float64); ok {
		r.AlertDays = int64(v)
	}
	if v, ok := vars["appendOnly"].(bool); ok {
		r.AppendOnly = v
	}
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func writeData(w http.ResponseWriter, data map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{"data": data})
}

func writeErr(w http.ResponseWriter, message string) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"errors": []map[string]any{{"message": message}},
	})
}

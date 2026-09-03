package controller

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/clevyr/borgbase-operator/internal/borgbase"
)

// fakeAPI is an in-memory stand-in for BorgBase that records every call, so
// tests can assert not just on the resulting state but on which operations the
// controller was willing to perform.
type fakeAPI struct {
	mu    sync.Mutex
	repos map[string]*borgbase.Repo

	// calls records operation names in order.
	calls []string

	// edits records the options passed to each Edit call.
	edits []borgbase.EditOptions

	// getErr, if set, is returned by Get instead of a lookup.
	getErr error
}

func newFakeAPI(repos ...*borgbase.Repo) *fakeAPI {
	f := &fakeAPI{repos: map[string]*borgbase.Repo{}}
	for _, r := range repos {
		f.repos[r.ID] = r
	}
	return f
}

func (f *fakeAPI) record(op string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls = append(f.calls, op)
}

func (f *fakeAPI) called(op string) bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Contains(f.calls, op)
}

func (f *fakeAPI) Get(_ context.Context, id string) (*borgbase.Repo, error) {
	f.record("Get")
	if f.getErr != nil {
		return nil, f.getErr
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	if r, ok := f.repos[id]; ok {
		return r, nil
	}
	return nil, fmt.Errorf("%w: %s", borgbase.ErrNotFound, id)
}

func (f *fakeAPI) FindByName(_ context.Context, name string) (*borgbase.Repo, error) {
	f.record("FindByName")
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, r := range f.repos {
		if r.Name == name {
			return r, nil
		}
	}
	return nil, fmt.Errorf("%w: name %s", borgbase.ErrNotFound, name)
}

func (f *fakeAPI) Add(_ context.Context, opts borgbase.AddOptions) (*borgbase.Repo, error) {
	f.record("Add")
	f.mu.Lock()
	defer f.mu.Unlock()
	id := fmt.Sprintf("new%04d", len(f.repos))
	r := &borgbase.Repo{
		ID:       id,
		Name:     opts.Name,
		Region:   opts.Region,
		Format:   borgbase.FormatRestic,
		Htpasswd: "token-" + id,
	}
	f.repos[id] = r
	return r, nil
}

// Edit applies the options, so a test can assert both that the controller
// asked for a change and that it asked for the right one.
func (f *fakeAPI) Edit(_ context.Context, id string, opts borgbase.EditOptions) (*borgbase.Repo, error) {
	f.record("Edit")
	f.mu.Lock()
	defer f.mu.Unlock()
	r, ok := f.repos[id]
	if !ok {
		return nil, borgbase.ErrNotFound
	}
	f.edits = append(f.edits, opts)
	if opts.QuotaEnabled != nil {
		r.QuotaEnabled = *opts.QuotaEnabled
	}
	if opts.Quota != nil {
		r.Quota = *opts.Quota
	}
	if opts.AlertDays != nil {
		r.AlertDays = *opts.AlertDays
	}
	if opts.AppendOnly != nil {
		r.AppendOnly = *opts.AppendOnly
	}
	return r, nil
}

// lastEdit returns the options of the most recent Edit call.
func (f *fakeAPI) lastEdit() borgbase.EditOptions {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(f.edits) == 0 {
		return borgbase.EditOptions{}
	}
	return f.edits[len(f.edits)-1]
}

func (f *fakeAPI) Delete(_ context.Context, id string) error {
	f.record("Delete")
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.repos, id)
	return nil
}

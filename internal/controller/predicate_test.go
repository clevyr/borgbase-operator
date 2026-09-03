package controller

import (
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/event"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

func repoWith(mutate func(*borgbasev1.Repository)) *borgbasev1.Repository {
	r := &borgbasev1.Repository{
		Name: resticName, Namespace: testNS, Generation: 1,
	}
	mutate(r)
	return r
}

func TestIgnoreOwnStatusWrites(t *testing.T) {
	now := metav1.Now()
	tests := []struct {
		name     string
		old, new *borgbasev1.Repository
		want     bool
	}{
		{
			name: "status-only change is filtered",
			old:  repoWith(func(r *borgbasev1.Repository) {}),
			new:  repoWith(func(r *borgbasev1.Repository) { r.Status.Initialized = true }),
			want: false,
		},
		{
			name: "spec change passes",
			old:  repoWith(func(r *borgbasev1.Repository) {}),
			new:  repoWith(func(r *borgbasev1.Repository) { r.Generation = 2 }),
			want: true,
		},
		{
			// Filtering this would strand the object in Terminating forever,
			// because the finalizer would never be reconciled away.
			name: "deletion always passes",
			old:  repoWith(func(r *borgbasev1.Repository) {}),
			new: repoWith(func(r *borgbasev1.Repository) {
				r.DeletionTimestamp = &now
				r.Finalizers = []string{FinalizerName}
			}),
			want: true,
		},
		{
			name: "finalizer change passes",
			old:  repoWith(func(r *borgbasev1.Repository) {}),
			new:  repoWith(func(r *borgbasev1.Repository) { r.Finalizers = []string{FinalizerName} }),
			want: true,
		},
	}

	p := ignoreOwnStatusWrites()
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := p.Update(event.UpdateEvent{
				ObjectOld: client.Object(tt.old),
				ObjectNew: client.Object(tt.new),
			})
			if got != tt.want {
				t.Errorf("Update() = %v, want %v", got, tt.want)
			}
		})
	}
}

// Creates and deletes must never be filtered.
func TestIgnoreOwnStatusWritesPassesCreateAndDelete(t *testing.T) {
	p := ignoreOwnStatusWrites()
	obj := repoWith(func(r *borgbasev1.Repository) {})
	if !p.Create(event.CreateEvent{Object: obj}) {
		t.Error("create events must not be filtered")
	}
	if !p.Delete(event.DeleteEvent{Object: obj}) {
		t.Error("delete events must not be filtered")
	}
}

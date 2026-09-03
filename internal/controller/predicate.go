package controller

import (
	"k8s.io/apimachinery/pkg/api/equality"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
)

// ignoreOwnStatusWrites skips update events that changed nothing but status.
//
// Writing status re-triggers the controller, and that pass reads from an
// informer cache which has not caught up with the write yet. It therefore
// re-observes work it has already done, duplicating the events that report it.
//
// Creates, deletes, spec changes and finalizer changes all still come through,
// so this cannot strand an object mid-deletion.
//
// Any annotation named in watchAnnotations also comes through. Annotations do
// not advance the generation, so without this an object whose only change is a
// request-carrying annotation would be filtered out and the request never
// acted on.
func ignoreOwnStatusWrites(watchAnnotations ...string) predicate.Predicate {
	return predicate.Funcs{
		UpdateFunc: func(e event.UpdateEvent) bool {
			if e.ObjectOld == nil || e.ObjectNew == nil {
				return true
			}
			// Never filter anything to do with teardown.
			if !e.ObjectNew.GetDeletionTimestamp().IsZero() {
				return true
			}
			if !equality.Semantic.DeepEqual(e.ObjectOld.GetFinalizers(), e.ObjectNew.GetFinalizers()) {
				return true
			}
			for _, key := range watchAnnotations {
				if e.ObjectOld.GetAnnotations()[key] != e.ObjectNew.GetAnnotations()[key] {
					return true
				}
			}
			// Generation only advances on spec changes, so an unchanged
			// generation means status is all that moved.
			return e.ObjectOld.GetGeneration() != e.ObjectNew.GetGeneration()
		},
	}
}

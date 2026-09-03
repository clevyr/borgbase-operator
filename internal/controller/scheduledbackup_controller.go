package controller

import (
	"context"
	"fmt"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/fields"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

// repositoryRefField indexes ScheduledBackups by the Repository they use, so a
// Repository becoming ready can requeue everything that depends on it.
const repositoryRefField = ".spec.repositoryRef.name"

// ScheduledBackupReconciler reconciles a ScheduledBackup object.
type ScheduledBackupReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// Config carries the operator-level backup defaults.
	Config backup.Config
}

// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=scheduledbackups,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=scheduledbackups/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=scheduledbackups/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=cronjobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=persistentvolumeclaims,verbs=get;create;delete

// Reconcile renders a ScheduledBackup into a CronJob and its cache volume.
func (r *ScheduledBackupReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var sb borgbasev1.ScheduledBackup
	if err := r.Get(ctx, req.NamespacedName, &sb); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	statusBase := sb.DeepCopy()

	result, err := r.reconcile(ctx, &sb)
	if err != nil {
		r.setCondition(&sb, metav1.Condition{
			Type:    borgbasev1.ScheduledBackupConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ReconcileFailed",
			Message: err.Error(),
		})
		r.Recorder.Eventf(&sb, nil, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &sb, statusBase); statusErr != nil {
			log.FromContext(ctx).Error(statusErr, "updating status after a failed reconcile")
		}
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &sb, statusBase); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *ScheduledBackupReconciler) reconcile(
	ctx context.Context, sb *borgbasev1.ScheduledBackup,
) (ctrl.Result, error) {
	var repo borgbasev1.Repository
	key := types.NamespacedName{Namespace: sb.Namespace, Name: sb.Spec.RepositoryRef.Name}
	if err := r.Get(ctx, key, &repo); err != nil {
		if apierrors.IsNotFound(err) {
			r.setCondition(sb, metav1.Condition{
				Type:    borgbasev1.ScheduledBackupConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "RepositoryNotFound",
				Message: fmt.Sprintf("Repository %q does not exist", sb.Spec.RepositoryRef.Name),
			})
			// A watch on Repository requeues this as soon as one appears.
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// Never schedule a backup against a repository that has not been
	// initialized: the run would fail, and on a fresh repository it could
	// initialize it with the wrong password.
	if !repo.Status.Initialized {
		r.setCondition(sb, metav1.Condition{
			Type:    borgbasev1.ScheduledBackupConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "RepositoryNotReady",
			Message: fmt.Sprintf("Repository %q is not initialized yet", repo.Name),
		})
		return ctrl.Result{}, nil
	}

	// Two backups reporting to one healthchecks check would let either one's
	// success hide the other's failure, which defeats the point of a dead
	// man's switch.
	if other, err := r.slugConflict(ctx, sb); err != nil {
		return ctrl.Result{}, err
	} else if other != "" {
		r.setCondition(sb, metav1.Condition{
			Type:   borgbasev1.ScheduledBackupConditionReady,
			Status: metav1.ConditionFalse,
			Reason: "SlugConflict",
			Message: fmt.Sprintf(
				"healthchecks slug %q is already reported by ScheduledBackup %q; "+
					"set spec.healthchecks.slug to something unique",
				backup.Reporter(sb, r.Config).Slug(), other),
		})
		return ctrl.Result{}, nil
	}

	if err := r.reconcileCache(ctx, sb); err != nil {
		return ctrl.Result{}, err
	}

	desired, err := backup.BuildCronJob(sb, &repo, r.Config)
	if err != nil {
		return ctrl.Result{}, err
	}
	if err := controllerutil.SetControllerReference(sb, desired, r.Scheme); err != nil {
		return ctrl.Result{}, err
	}

	var current batchv1.CronJob
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	switch {
	case apierrors.IsNotFound(err):
		if err := r.Create(ctx, desired); err != nil {
			if apierrors.IsAlreadyExists(err) {
				// The cache only holds CronJobs this operator labelled, so a
				// name taken by something else looks absent right up until the
				// create fails. Say so, rather than looping on a bare conflict.
				return ctrl.Result{}, fmt.Errorf(
					"CronJob %s already exists but is not managed by this operator; "+
						"remove it or rename the ScheduledBackup", desired.Name)
			}
			return ctrl.Result{}, fmt.Errorf("creating cronjob: %w", err)
		}
		r.Recorder.Eventf(sb, nil, corev1.EventTypeNormal, "CronJobCreated", "Create",
			"Created CronJob %s on schedule %s", desired.Name, desired.Spec.Schedule)
	case err != nil:
		return ctrl.Result{}, err
	default:
		// Overwrite the spec wholesale so that manual edits are corrected;
		// the CronJob is entirely owned by this resource.
		updated := current.DeepCopy()
		updated.Spec = desired.Spec
		updated.Labels = desired.Labels
		updated.OwnerReferences = desired.OwnerReferences
		if err := r.Update(ctx, updated); err != nil {
			return ctrl.Result{}, fmt.Errorf("updating cronjob: %w", err)
		}
		sb.Status.LastScheduleTime = current.Status.LastScheduleTime
		sb.Status.LastSuccessfulTime = current.Status.LastSuccessfulTime
		sb.Status.Active = int32(len(current.Status.Active))
	}

	sb.Status.EffectiveSchedule = desired.Spec.Schedule
	r.setCondition(sb, metav1.Condition{
		Type:    borgbasev1.ScheduledBackupConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: fmt.Sprintf("Backing up on schedule %s", desired.Spec.Schedule),
	})

	// Poll so that lastSuccessfulTime stays fresh even though CronJob status
	// changes do not always generate a watch event this controller sees.
	return ctrl.Result{RequeueAfter: 5 * time.Minute}, nil
}

// reconcileCache creates the restic cache volume if it does not exist.
//
// The claim is never resized or replaced in place: a bound PVC is immutable in
// the ways that matter here, and silently deleting one would throw away the
// cache the next prune depends on.
func (r *ScheduledBackupReconciler) reconcileCache(
	ctx context.Context, sb *borgbasev1.ScheduledBackup,
) error {
	desired, err := backup.BuildCachePVC(sb, r.Config)
	if err != nil {
		return err
	}
	if desired == nil {
		return r.deleteCache(ctx, sb)
	}
	if err := controllerutil.SetControllerReference(sb, desired, r.Scheme); err != nil {
		return err
	}

	var current corev1.PersistentVolumeClaim
	err = r.Get(ctx, types.NamespacedName{Namespace: desired.Namespace, Name: desired.Name}, &current)
	if apierrors.IsNotFound(err) {
		if err := r.Create(ctx, desired); err != nil && !apierrors.IsAlreadyExists(err) {
			return fmt.Errorf("creating cache volume: %w", err)
		}
		return nil
	}
	return err
}

// deleteCache removes a cache volume the operator created, once the cache has
// been turned off. A claim the operator does not own is left alone.
func (r *ScheduledBackupReconciler) deleteCache(
	ctx context.Context, sb *borgbasev1.ScheduledBackup,
) error {
	var pvc corev1.PersistentVolumeClaim
	key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CacheName(sb)}
	if err := r.Get(ctx, key, &pvc); err != nil {
		return client.IgnoreNotFound(err)
	}
	if owner := metav1.GetControllerOf(&pvc); owner == nil || owner.UID != sb.UID {
		return nil
	}
	if err := r.Delete(ctx, &pvc); err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting cache volume: %w", err)
	}
	return nil
}

// slugConflict returns the name of another ScheduledBackup in this namespace
// that already pings the same healthchecks check, or "" when there is none.
//
// Only a backup that was created earlier counts as the incumbent, so the one
// already reporting keeps working and the newcomer is the one held back.
func (r *ScheduledBackupReconciler) slugConflict(
	ctx context.Context, sb *borgbasev1.ScheduledBackup,
) (string, error) {
	mine := backup.Reporter(sb, r.Config)
	if !mine.Enabled() || mine.PingsByUUID() {
		return "", nil
	}

	var list borgbasev1.ScheduledBackupList
	if err := r.List(ctx, &list, client.InNamespace(sb.Namespace)); err != nil {
		return "", fmt.Errorf("listing backups in %s: %w", sb.Namespace, err)
	}

	for i := range list.Items {
		other := &list.Items[i]
		if other.Name == sb.Name || !other.DeletionTimestamp.IsZero() {
			continue
		}
		theirs := backup.Reporter(other, r.Config)
		if !theirs.Enabled() || theirs.PingsByUUID() || theirs.Slug() != mine.Slug() {
			continue
		}
		if olderThan(other, sb) {
			return other.Name, nil
		}
	}
	return "", nil
}

// olderThan reports whether a was created before b, falling back to name order
// so that two objects created in the same second still resolve consistently.
func olderThan(a, b *borgbasev1.ScheduledBackup) bool {
	if !a.CreationTimestamp.Equal(&b.CreationTimestamp) {
		return a.CreationTimestamp.Before(&b.CreationTimestamp)
	}
	return a.Name < b.Name
}

// patchStatus writes the computed status as a merge patch, avoiding the
// resourceVersion conflicts a full Update hits when reading from a lagging
// informer cache.
func (r *ScheduledBackupReconciler) patchStatus(
	ctx context.Context, sb *borgbasev1.ScheduledBackup, base *borgbasev1.ScheduledBackup,
) error {
	sb.Status.ObservedGeneration = sb.Generation
	if equality.Semantic.DeepEqual(sb.Status, base.Status) {
		return nil
	}
	return r.Status().Patch(ctx, sb, client.MergeFrom(base))
}

func (r *ScheduledBackupReconciler) setCondition(
	sb *borgbasev1.ScheduledBackup, cond metav1.Condition,
) {
	cond.ObservedGeneration = sb.Generation
	apimeta.SetStatusCondition(&sb.Status.Conditions, cond)
}

// SetupWithManager sets up the controller with the Manager.
func (r *ScheduledBackupReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("scheduledbackup-controller")
	}

	if err := mgr.GetFieldIndexer().IndexField(
		context.Background(), &borgbasev1.ScheduledBackup{}, repositoryRefField,
		func(o client.Object) []string {
			sb, ok := o.(*borgbasev1.ScheduledBackup)
			if !ok || sb.Spec.RepositoryRef.Name == "" {
				return nil
			}
			return []string{sb.Spec.RepositoryRef.Name}
		},
	); err != nil {
		return err
	}

	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		For(&borgbasev1.ScheduledBackup{}, builder.WithPredicates(ignoreOwnStatusWrites())).
		Owns(&batchv1.CronJob{}).
		Watches(
			&borgbasev1.Repository{},
			handler.EnqueueRequestsFromMapFunc(r.backupsForRepository),
		).
		Named("scheduledbackup").
		Complete(r)
}

// backupsForRepository requeues every ScheduledBackup that uses a Repository,
// so that a repository finishing initialization immediately unblocks them.
func (r *ScheduledBackupReconciler) backupsForRepository(
	ctx context.Context, obj client.Object,
) []reconcile.Request {
	var list borgbasev1.ScheduledBackupList
	if err := r.List(ctx, &list,
		client.InNamespace(obj.GetNamespace()),
		client.MatchingFieldsSelector{
			Selector: fields.OneTermEqualSelector(repositoryRefField, obj.GetName()),
		},
	); err != nil {
		log.FromContext(ctx).Error(err, "listing backups for repository", "repository", obj.GetName())
		return nil
	}

	requests := make([]reconcile.Request, 0, len(list.Items))
	for _, sb := range list.Items {
		requests = append(requests, reconcile.Request{
			Namespace: sb.Namespace, Name: sb.Name,
		})
	}
	return requests
}

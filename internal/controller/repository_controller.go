package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/borgbase"
	"github.com/clevyr/borgbase-operator/internal/secrets"
)

const (
	// FinalizerName guards deletion so that the BorgBase repository is handled
	// according to the deletion policy before the object disappears.
	FinalizerName = "borgbase.clevyr.com/finalizer"

	// Keys written into the credentials Secret. These names are what restic
	// itself reads from the environment.
	KeyResticRepository = "RESTIC_REPOSITORY"
	KeyResticPassword   = "RESTIC_PASSWORD"

	initJobSuffix = "-init"
)

// RepositoryReconciler reconciles a Repository object.
type RepositoryReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder events.EventRecorder

	// NewAPI builds a BorgBase client for a token. It is a field so tests can
	// substitute a fake without reaching the network.
	NewAPI func(token string) borgbase.API

	// DefaultTokenSecret is the fallback BorgBase API token, used when a
	// Repository does not name its own.
	DefaultTokenSecret types.NamespacedName
	DefaultTokenKey    string

	// BackupImage runs the init Job. It must provide restic.
	BackupImage string

	// Endpoint overrides the BorgBase GraphQL endpoint. Empty uses the public
	// API; it exists so the operator can be pointed at a stub in tests.
	Endpoint string
}

// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups="",resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=events.k8s.io,resources=events,verbs=create;patch
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete

// Reconcile brings a BorgBase repository and its credentials Secret in line
// with the Repository spec.
func (r *RepositoryReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx)

	var repo borgbasev1.Repository
	if err := r.Get(ctx, req.NamespacedName, &repo); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	api, err := r.apiFor(ctx, &repo)
	if err != nil {
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "APITokenUnavailable", "Reconcile", "%s", err.Error())
		return ctrl.Result{}, err
	}

	if !repo.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, r.finalize(ctx, &repo, api)
	}

	if !controllerutil.ContainsFinalizer(&repo, FinalizerName) {
		controllerutil.AddFinalizer(&repo, FinalizerName)
		if err := r.Update(ctx, &repo); err != nil {
			return ctrl.Result{}, err
		}
	}

	if repo.Spec.Suspend {
		logger.V(1).Info("reconciliation suspended")
		return ctrl.Result{}, nil
	}

	result, err := r.reconcile(ctx, &repo, api)
	if err != nil {
		meta := metav1.Condition{
			Type:    borgbasev1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "ReconcileFailed",
			Message: err.Error(),
		}
		r.setCondition(&repo, meta)
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "ReconcileFailed", "Reconcile", "%s", err.Error())
		if statusErr := r.updateStatus(ctx, &repo); statusErr != nil {
			logger.Error(statusErr, "updating status after a failed reconcile")
		}
		return ctrl.Result{}, err
	}

	if err := r.updateStatus(ctx, &repo); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *RepositoryReconciler) reconcile(
	ctx context.Context, repo *borgbasev1.Repository, api borgbase.API,
) (ctrl.Result, error) {
	remote, err := r.resolveRepo(ctx, repo, api)
	if err != nil {
		return ctrl.Result{}, err
	}

	// A repository's format is fixed when it is created, so a non-restic repo
	// can never be brought into line. Fail loudly rather than writing a
	// RESTIC_REPOSITORY URL that cannot work.
	if !remote.IsRestic() {
		return ctrl.Result{}, fmt.Errorf(
			"%w: repository %s has format %q", borgbase.ErrNotRestic, remote.ID, remote.Format)
	}

	repo.Status.RepositoryID = remote.ID
	repo.Status.Server = remote.Host()
	repo.Status.CurrentUsage = formatBytes(remote.CurrentUsage)
	if remote.QuotaEnabled {
		repo.Status.Quota = formatGiB(remote.Quota)
	} else {
		repo.Status.Quota = ""
	}

	if err := r.reconcileSecret(ctx, repo, remote); err != nil {
		return ctrl.Result{}, err
	}

	if !repo.Status.Initialized {
		requeue, err := r.reconcileInitJob(ctx, repo)
		if err != nil {
			return ctrl.Result{}, err
		}
		if requeue > 0 {
			r.setCondition(repo, metav1.Condition{
				Type:    borgbasev1.RepositoryConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "Initializing",
				Message: "waiting for restic init to complete",
			})
			return ctrl.Result{RequeueAfter: requeue}, nil
		}
	}

	r.setCondition(repo, metav1.Condition{
		Type:    borgbasev1.RepositoryConditionInitialized,
		Status:  metav1.ConditionTrue,
		Reason:  "Initialized",
		Message: "repository is initialized",
	})
	r.setCondition(repo, metav1.Condition{
		Type:    borgbasev1.RepositoryConditionReady,
		Status:  metav1.ConditionTrue,
		Reason:  "Ready",
		Message: "repository is ready",
	})

	return ctrl.Result{RequeueAfter: r.interval(repo)}, nil
}

// resolveRepo finds the BorgBase repository this resource refers to, creating
// one only when the spec asks for creation.
func (r *RepositoryReconciler) resolveRepo(
	ctx context.Context, repo *borgbasev1.Repository, api borgbase.API,
) (*borgbase.Repo, error) {
	// Adoption: look up and never create. A wrong ID must surface as an error
	// rather than quietly provisioning an empty repository beside real
	// backups, so there is no fallback to creation on this path.
	if id := repo.Spec.ExistingRepositoryID; id != "" {
		remote, err := api.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("adopting repository %s: %w", id, err)
		}
		repo.Status.Adopted = true
		return remote, nil
	}

	// Already created by a previous reconcile: look it up by the recorded ID so
	// that renaming the resource cannot orphan the repository.
	if id := repo.Status.RepositoryID; id != "" {
		remote, err := api.Get(ctx, id)
		if err != nil {
			return nil, fmt.Errorf("looking up repository %s: %w", id, err)
		}
		return remote, nil
	}

	name := repo.RepositoryName()
	remote, err := api.FindByName(ctx, name)
	switch {
	case err == nil:
		return remote, nil
	case !errors.Is(err, borgbase.ErrNotFound):
		return nil, err
	}

	remote, err = api.Add(ctx, borgbase.AddOptions{
		Name:       name,
		Region:     repo.Spec.Region,
		Quota:      quotaBytes(repo.Spec.QuotaGiB),
		AlertDays:  int64Ptr(repo.Spec.AlertDays),
		AppendOnly: repo.Spec.AppendOnly,
	})
	if err != nil {
		return nil, fmt.Errorf("creating repository %q: %w", name, err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryCreated", "Create",
		"Created BorgBase repository %s (%s)", remote.ID, name)
	return remote, nil
}

// reconcileSecret writes the credentials Secret.
//
// The restic password is the encryption key for every snapshot in the
// repository. Once a password exists anywhere it is reused verbatim and never
// regenerated: rotating it would render every existing backup unreadable.
func (r *RepositoryReconciler) reconcileSecret(
	ctx context.Context, repo *borgbasev1.Repository, remote *borgbase.Repo,
) error {
	url, err := remote.ResticURL()
	if err != nil {
		return err
	}

	name := repo.SecretName()
	var existing corev1.Secret
	err = r.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &existing)
	switch {
	case err == nil:
	case apierrors.IsNotFound(err):
		existing = corev1.Secret{}
	default:
		return err
	}

	password := string(existing.Data[KeyResticPassword])
	generated := false

	if password == "" && repo.Spec.PasswordSecretRef != nil {
		password, err = r.readSecretKey(ctx, repo.Namespace, repo.Spec.PasswordSecretRef)
		if err != nil {
			return fmt.Errorf("reading passwordSecretRef: %w", err)
		}
	}

	if password == "" {
		// Refuse to invent a password for a repository that already holds
		// data: its snapshots were written under a password we do not have,
		// and a new one would not decrypt them.
		if repo.Status.Adopted || remote.CurrentUsage > 0 {
			return fmt.Errorf(
				"repository %s already contains data but no password is available; "+
					"set spec.passwordSecretRef to the existing RESTIC_PASSWORD", remote.ID)
		}
		if password, err = secrets.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	}

	desired := &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: repo.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/managed-by": "borgbase-operator",
			},
		},
		Type: corev1.SecretTypeOpaque,
		// Written as Data rather than StringData: StringData is write-only,
		// so using it would make the create path and the read-back path
		// disagree about where the password lives.
		Data: map[string][]byte{
			KeyResticRepository: []byte(url),
			KeyResticPassword:   []byte(password),
		},
	}

	// Only take ownership when the repository is disposable. With the default
	// Retain policy the Secret must outlive the resource, because it is the
	// only copy of the key needed to read the backups.
	if repo.Spec.DeletionPolicy == borgbasev1.DeletionPolicyDelete {
		if err := controllerutil.SetControllerReference(repo, desired, r.Scheme); err != nil {
			return err
		}
	}

	op, err := r.createOrPatch(ctx, desired, &existing)
	if err != nil {
		return err
	}
	if generated {
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "PasswordGenerated", "Generate",
			"Generated a new restic password in Secret %s; back it up, it cannot be recovered", name)
	}
	if op != "" {
		log.FromContext(ctx).Info("reconciled credentials secret", "secret", name, "operation", op)
	}

	repo.Status.SecretName = name
	return nil
}

// createOrPatch creates the Secret or updates it in place, returning the
// operation performed or "" when nothing changed.
func (r *RepositoryReconciler) createOrPatch(
	ctx context.Context, desired *corev1.Secret, existing *corev1.Secret,
) (string, error) {
	if existing.Name == "" {
		if err := r.Create(ctx, desired); err != nil {
			return "", fmt.Errorf("creating secret %s: %w", desired.Name, err)
		}
		return "created", nil
	}

	unchanged := true
	for k, v := range desired.Data {
		if string(existing.Data[k]) != string(v) {
			unchanged = false
			break
		}
	}
	if unchanged && len(existing.OwnerReferences) == len(desired.OwnerReferences) {
		return "", nil
	}

	updated := existing.DeepCopy()
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	maps.Copy(updated.Data, desired.Data)
	updated.OwnerReferences = desired.OwnerReferences
	if err := r.Update(ctx, updated); err != nil {
		return "", fmt.Errorf("updating secret %s: %w", desired.Name, err)
	}
	return "updated", nil
}

// reconcileInitJob runs `restic init` against the repository, returning how
// long to wait before checking again, or 0 once initialization has succeeded.
func (r *RepositoryReconciler) reconcileInitJob(
	ctx context.Context, repo *borgbasev1.Repository,
) (time.Duration, error) {
	name := repo.Name + initJobSuffix
	key := types.NamespacedName{Namespace: repo.Namespace, Name: name}

	var job batchv1.Job
	err := r.Get(ctx, key, &job)
	switch {
	case err == nil:
		if job.Status.Succeeded > 0 {
			repo.Status.Initialized = true
			r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "Initialized", "Initialize",
				"restic init completed successfully")
			return 0, nil
		}
		if isJobFailed(&job) {
			// Remove the exhausted Job so the next reconcile starts a fresh
			// one; without this a transient failure would wedge permanently.
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "InitFailed", "Initialize",
				"restic init failed; retrying")
			policy := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, &job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
				!apierrors.IsNotFound(err) {
				return 0, err
			}
			return 5 * time.Minute, nil
		}
		return 15 * time.Second, nil

	case !apierrors.IsNotFound(err):
		return 0, err
	}

	job = r.buildInitJob(repo, name)
	if err := controllerutil.SetControllerReference(repo, &job, r.Scheme); err != nil {
		return 0, err
	}
	if err := r.Create(ctx, &job); err != nil {
		if apierrors.IsAlreadyExists(err) {
			return 15 * time.Second, nil
		}
		return 0, fmt.Errorf("creating init job: %w", err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "Initializing", "Initialize", "Started init job %s", name)
	return 15 * time.Second, nil
}

func (r *RepositoryReconciler) buildInitJob(repo *borgbasev1.Repository, name string) batchv1.Job {
	return batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: repo.Namespace,
			Labels: map[string]string{
				"app.kubernetes.io/name":       repo.Name,
				"app.kubernetes.io/component":  "init",
				"app.kubernetes.io/managed-by": "borgbase-operator",
			},
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(3)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext: &corev1.PodSecurityContext{
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Containers: []corev1.Container{{
						Name:  "init",
						Image: r.BackupImage,
						// Probe before initializing. `restic init || true`
						// would also swallow genuine failures such as bad
						// credentials, reporting success for a repository that
						// no backup can ever write to.
						Command: []string{"sh", "-c", "restic cat config >/dev/null 2>&1 || restic init"},
						EnvFrom: []corev1.EnvFromSource{{
							SecretRef: &corev1.SecretEnvSource{
								LocalObjectReference: corev1.LocalObjectReference{Name: repo.SecretName()},
							},
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
						},
						// Without requests the pod is BestEffort and is the
						// first thing evicted under node pressure, which would
						// leave a repository stuck uninitialized.
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse("10m"),
								corev1.ResourceMemory: resource.MustParse("64Mi"),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceMemory: resource.MustParse("256Mi"),
							},
						},
					}},
				},
			},
		},
	}
}

func isJobFailed(job *batchv1.Job) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// finalize handles deletion according to the deletion policy.
func (r *RepositoryReconciler) finalize(
	ctx context.Context, repo *borgbasev1.Repository, api borgbase.API,
) error {
	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return nil
	}

	if repo.Spec.DeletionPolicy == borgbasev1.DeletionPolicyDelete && repo.Status.RepositoryID != "" {
		if err := api.Delete(ctx, repo.Status.RepositoryID); err != nil && !errors.Is(err, borgbase.ErrNotFound) {
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "DeleteFailed", "Delete", "%s", err.Error())
			return fmt.Errorf("deleting repository %s: %w", repo.Status.RepositoryID, err)
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
			"Deleted BorgBase repository %s and all its snapshots", repo.Status.RepositoryID)
	} else if repo.Status.RepositoryID != "" {
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryRetained", "Retain",
			"Retained BorgBase repository %s; delete it manually if it is no longer needed",
			repo.Status.RepositoryID)
	}

	controllerutil.RemoveFinalizer(repo, FinalizerName)
	return r.Update(ctx, repo)
}

// apiFor builds a BorgBase client using the repository's token or the default.
func (r *RepositoryReconciler) apiFor(
	ctx context.Context, repo *borgbasev1.Repository,
) (borgbase.API, error) {
	ref := repo.Spec.APITokenSecretRef
	namespace, selector := repo.Namespace, ref
	if ref == nil {
		namespace = r.DefaultTokenSecret.Namespace
		selector = &corev1.SecretKeySelector{
			LocalObjectReference: corev1.LocalObjectReference{Name: r.DefaultTokenSecret.Name},
			Key:                  r.DefaultTokenKey,
		}
	}

	token, err := r.readSecretKey(ctx, namespace, selector)
	if err != nil {
		return nil, fmt.Errorf("reading BorgBase API token: %w", err)
	}
	newAPI := r.NewAPI
	if newAPI == nil {
		newAPI = func(t string) borgbase.API {
			c := borgbase.NewClient(t)
			if r.Endpoint != "" {
				c.Endpoint = r.Endpoint
			}
			return c
		}
	}
	return newAPI(token), nil
}

func (r *RepositoryReconciler) readSecretKey(
	ctx context.Context, namespace string, ref *corev1.SecretKeySelector,
) (string, error) {
	if ref == nil || ref.Name == "" {
		return "", fmt.Errorf("no secret reference configured")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, &secret); err != nil {
		return "", err
	}
	value, ok := secret.Data[ref.Key]
	if !ok || len(value) == 0 {
		return "", fmt.Errorf("secret %s/%s has no key %q", namespace, ref.Name, ref.Key)
	}
	return string(value), nil
}

func (r *RepositoryReconciler) interval(repo *borgbasev1.Repository) time.Duration {
	if repo.Spec.Interval != nil && repo.Spec.Interval.Duration > 0 {
		return repo.Spec.Interval.Duration
	}
	return time.Hour
}

func (r *RepositoryReconciler) setCondition(repo *borgbasev1.Repository, cond metav1.Condition) {
	cond.ObservedGeneration = repo.Generation
	apimeta.SetStatusCondition(&repo.Status.Conditions, cond)
}

func (r *RepositoryReconciler) updateStatus(ctx context.Context, repo *borgbasev1.Repository) error {
	repo.Status.ObservedGeneration = repo.Generation
	return r.Status().Update(ctx, repo)
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("repository-controller")
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&borgbasev1.Repository{}).
		// Secrets are deliberately not owned or watched: doing so would build
		// an informer over every Secret in the cluster. A credentials Secret
		// deleted out from under us is recreated on the next periodic
		// reconcile rather than instantly.
		Owns(&batchv1.Job{}).
		Named("repository").
		Complete(r)
}

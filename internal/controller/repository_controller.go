package controller

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"slices"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	apimeta "k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/events"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	crcontroller "sigs.k8s.io/controller-runtime/pkg/controller"
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

	// initCleanupDelay is how long to wait after recording initialization
	// before removing the init Job, so the status write reaches the cache first.
	initCleanupDelay = 10 * time.Second

	// initRetryDelay paces replacement of a failed init Job, and keeps its logs
	// around for that long.
	initRetryDelay = 5 * time.Minute

	// maxConcurrentReconciles applies to both controllers.
	//
	// Reconciling a Repository blocks on the BorgBase API, and that API is not
	// fast: deleting a repository has been measured at over eleven seconds. With
	// a single worker one slow or hung call stalls every other repository behind
	// it. controller-runtime still serialises reconciles per object, so the only
	// thing this changes is that unrelated objects make progress independently.
	maxConcurrentReconciles = 4
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

	// APIReader reads straight from the API server, bypassing the informer
	// cache. It is used only where a stale read would cause visible duplicate
	// work; everything else goes through the cached client.
	APIReader client.Reader

	// Endpoint overrides the BorgBase GraphQL endpoint. Empty uses the public
	// API; it exists so the operator can be pointed at a stub in tests.
	Endpoint string
}

// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=borgbase.clevyr.com,resources=repositories/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;create;update;patch
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

	// Deletion is handled before the API token is resolved. Under the default
	// Retain policy nothing calls BorgBase at all, so a missing or rotated
	// token must not be able to strand the object in Terminating.
	if !repo.DeletionTimestamp.IsZero() {
		statusBase := repo.DeepCopy()
		if err := r.finalize(ctx, &repo); err != nil {
			if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
				logger.Error(statusErr, "Failed to update status after a failed finalize")
			}
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&repo, FinalizerName) {
		patch := client.MergeFrom(repo.DeepCopy())
		controllerutil.AddFinalizer(&repo, FinalizerName)
		if err := r.Patch(ctx, &repo, patch); err != nil {
			return ctrl.Result{}, err
		}
	}

	// Captured after the finalizer patch so the status patch is computed
	// against what the object looks like now.
	statusBase := repo.DeepCopy()

	// Suspending also predates the token lookup: the point of it is to stop the
	// controller touching anything while a problem is investigated.
	if repo.Spec.Suspend {
		logger.V(1).Info("Reconciliation suspended")
		r.setCondition(&repo, metav1.Condition{
			Type:    borgbasev1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "Suspended",
			Message: "reconciliation is paused by spec.suspend",
		})
		return ctrl.Result{}, r.patchStatus(ctx, &repo, statusBase)
	}

	api, err := r.apiFor(ctx, &repo)
	if err != nil {
		r.setCondition(&repo, metav1.Condition{
			Type:    borgbasev1.RepositoryConditionReady,
			Status:  metav1.ConditionFalse,
			Reason:  "APITokenUnavailable",
			Message: err.Error(),
		})
		r.Recorder.Eventf(&repo, nil, corev1.EventTypeWarning, "APITokenUnavailable", "Reconcile", "%s", err.Error())
		if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after an API token failure")
		}
		return ctrl.Result{}, err
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
		if statusErr := r.patchStatus(ctx, &repo, statusBase); statusErr != nil {
			logger.Error(statusErr, "Failed to update status after a failed reconcile")
		}
		return ctrl.Result{}, err
	}

	if err := r.patchStatus(ctx, &repo, statusBase); err != nil {
		return ctrl.Result{}, err
	}
	return result, nil
}

func (r *RepositoryReconciler) reconcile(
	ctx context.Context, repo *borgbasev1.Repository, api borgbase.API,
) (ctrl.Result, error) {
	// Read before status is overwritten below. This is the only evidence that
	// the resource has already been through a successful pass, and therefore
	// that a password exists somewhere even if the Secret has gone missing.
	known := repo.Status.RepositoryID != "" || repo.Status.Initialized

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

	if remote, err = r.reconcileSettings(ctx, repo, remote, api); err != nil {
		return ctrl.Result{}, err
	}

	repo.Status.RepositoryID = remote.ID
	repo.Status.Server = remote.Host()
	repo.Status.CurrentUsage = formatUsage(remote.CurrentUsage)
	if remote.QuotaEnabled {
		repo.Status.Quota = formatQuota(remote.Quota)
	} else {
		repo.Status.Quota = ""
	}

	if err := r.reconcileSecret(ctx, repo, remote, known); err != nil {
		return ctrl.Result{}, err
	}

	justInitialized := false
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
		justInitialized = true
	} else if err := r.cleanupInitJob(ctx, repo); err != nil {
		return ctrl.Result{}, err
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

	if justInitialized {
		// Come back shortly to remove the finished Job, rather than deleting it
		// here. Deleting it in this pass fires a watch event that can be
		// handled before this status write reaches the informer cache, and that
		// reconcile would see Initialized false with no Job and start another.
		return ctrl.Result{RequeueAfter: initCleanupDelay}, nil
	}

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
		// The CRD forbids changing this field, but an object written before
		// that rule existed, or restored from a backup of etcd, can still
		// disagree with what was recorded. Repointing would orphan the
		// snapshots the recorded repository holds.
		if recorded := repo.Status.RepositoryID; recorded != "" && recorded != id {
			return nil, fmt.Errorf(
				"spec.existingRepositoryID is %s but this resource already manages repository %s; "+
					"delete and recreate the Repository if the change is intended", id, recorded)
		}
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
		Quota:      int64Ptr(repo.Spec.QuotaGiB),
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

// reconcileSettings brings the repository's mutable settings in line with the
// spec, and returns the repository as it stands afterwards.
//
// Only fields that actually differ are sent: repoEdit applies whatever it
// receives, so sending everything on every pass would overwrite a setting made
// in the BorgBase UI that the spec says nothing about.
func (r *RepositoryReconciler) reconcileSettings(
	ctx context.Context, repo *borgbasev1.Repository, remote *borgbase.Repo, api borgbase.API,
) (*borgbase.Repo, error) {
	var opts borgbase.EditOptions

	wantQuota := repo.Spec.QuotaGiB != nil
	if wantQuota != remote.QuotaEnabled {
		opts.QuotaEnabled = ptr.To(wantQuota)
	}
	if wantQuota {
		// Send the value alongside whenever the quota is being switched on, so
		// the repository is never left enabled with a stale limit.
		if q := int64(*repo.Spec.QuotaGiB); q != remote.Quota || opts.QuotaEnabled != nil {
			opts.Quota = ptr.To(q)
		}
	}
	if days := int64Ptr(repo.Spec.AlertDays); days != nil && *days != remote.AlertDays {
		opts.AlertDays = days
	}
	if repo.Spec.AppendOnly != remote.AppendOnly {
		opts.AppendOnly = ptr.To(repo.Spec.AppendOnly)
	}

	if opts.IsZero() {
		return remote, nil
	}

	updated, err := api.Edit(ctx, remote.ID, opts)
	if err != nil {
		return nil, fmt.Errorf("updating settings on repository %s: %w", remote.ID, err)
	}
	r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "SettingsUpdated", "Edit",
		"Updated settings on BorgBase repository %s", remote.ID)
	return updated, nil
}

// reconcileSecret writes the credentials Secret.
//
// The restic password is the encryption key for every snapshot in the
// repository. Once a password exists anywhere it is reused verbatim and never
// regenerated: rotating it would render every existing backup unreadable.
//
// known says whether this resource has already completed a pass, and therefore
// whether a password must already exist somewhere even though this Secret does
// not currently hold one.
func (r *RepositoryReconciler) reconcileSecret(
	ctx context.Context, repo *borgbasev1.Repository, remote *borgbase.Repo, known bool,
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
		// Refuse to invent a password for a repository that has been used
		// before: its snapshots were written under a password we do not have,
		// and a new one would not decrypt them. Usage is not enough on its own,
		// because a freshly initialized repository still reports zero.
		if repo.Status.Adopted || known || remote.CurrentUsage > 0 {
			return fmt.Errorf(
				"repository %s has already been provisioned but no password is available; "+
					"restore Secret %s or set spec.passwordSecretRef to the existing RESTIC_PASSWORD",
				remote.ID, name)
		}
		if password, err = secrets.GeneratePassword(); err != nil {
			return err
		}
		generated = true
	}

	desired := &corev1.Secret{
		Name:      name,
		Namespace: repo.Namespace,
		Labels: map[string]string{
			labelManagedBy: managedByValue,
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
		log.FromContext(ctx).Info("Reconciled credentials Secret", "secret", name, "operation", op)
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

	// Never write into a Secret this operator did not create. spec.secretName
	// is free-form, so a typo could otherwise point it at the SOPS seed or an
	// app's own credentials, and under the Delete policy an ownerReference
	// would then garbage collect them.
	if existing.Labels[labelManagedBy] != managedByValue {
		return "", fmt.Errorf(
			"secret %s/%s already exists and is not managed by this operator; "+
				"set spec.secretName to a name the operator owns",
			existing.Namespace, existing.Name)
	}

	updated := existing.DeepCopy()
	if updated.Labels == nil {
		updated.Labels = map[string]string{}
	}
	maps.Copy(updated.Labels, desired.Labels)
	if updated.Data == nil {
		updated.Data = map[string][]byte{}
	}
	maps.Copy(updated.Data, desired.Data)
	syncControllerRef(updated, desired)

	if equality.Semantic.DeepEqual(existing, updated) {
		return "", nil
	}
	if err := r.Update(ctx, updated); err != nil {
		return "", fmt.Errorf("updating secret %s: %w", desired.Name, err)
	}
	return "updated", nil
}

// syncControllerRef makes obj's controller reference match desired's, leaving
// any other ownerReference alone.
//
// The deletion policy can be flipped either way, so this both adds the
// reference and removes it. Comparing the references themselves rather than
// counting them is what makes the removal happen at all.
func syncControllerRef(obj *corev1.Secret, desired *corev1.Secret) {
	want := metav1.GetControllerOf(desired)
	have := metav1.GetControllerOf(obj)

	switch {
	case want == nil && have == nil:
		return
	case want != nil && have != nil && have.UID == want.UID && have.Name == want.Name:
		return
	}

	refs := slices.DeleteFunc(slices.Clone(obj.OwnerReferences), func(o metav1.OwnerReference) bool {
		return o.Controller != nil && *o.Controller
	})
	if want != nil {
		refs = append(refs, *want)
	}
	obj.OwnerReferences = refs
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
			// Deliberately not deleted here; cleanupInitJob removes it on a
			// later pass, once Initialized has reached the cache.
			//
			// Whether to report this is decided against the API server rather
			// than the cache: the Job watch fires more than once as the Job
			// settles, and those reconciles read a cache that has not caught up
			// with our own status write, so they would report success again.
			recorded, err := r.initializationRecorded(ctx, repo)
			if err != nil {
				return 0, err
			}
			repo.Status.Initialized = true
			if !recorded {
				r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "Initialized", "Initialize",
					"restic init completed successfully")
			}
			return 0, nil
		}
		if failedAt := jobFailedAt(&job); failedAt != nil {
			// Wait before replacing it. Deleting the exhausted Job immediately
			// fires a watch event that recreates it at once, turning the retry
			// into a hot loop against a repository that is already failing.
			r.setInitializing(repo, "InitFailed", fmt.Sprintf(
				"restic init failed in Job %s; on an adopted repository this usually means "+
					"passwordSecretRef holds the wrong password. Retrying in %s", name, initRetryDelay))
			if wait := initRetryDelay - time.Since(*failedAt); wait > 0 {
				return wait, nil
			}
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "InitFailed", "Initialize",
				"restic init failed in Job %s; if this repository was adopted, check that "+
					"passwordSecretRef holds its original password. Retrying", name)
			if err := r.deleteJob(ctx, &job); err != nil {
				return 0, err
			}
			return initRetryDelay, nil
		}
		r.setInitializing(repo, "Initializing", fmt.Sprintf("waiting for init Job %s to complete", name))
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
	r.setInitializing(repo, "Initializing", fmt.Sprintf("started init Job %s", name))
	return 15 * time.Second, nil
}

// setInitializing records that the repository is not yet initialized, so the
// condition says something before `restic init` has succeeded rather than being
// absent until it does.
func (r *RepositoryReconciler) setInitializing(repo *borgbasev1.Repository, reason, message string) {
	r.setCondition(repo, metav1.Condition{
		Type:    borgbasev1.RepositoryConditionInitialized,
		Status:  metav1.ConditionFalse,
		Reason:  reason,
		Message: message,
	})
}

func (r *RepositoryReconciler) buildInitJob(repo *borgbasev1.Repository, name string) batchv1.Job {
	return batchv1.Job{
		Name:      name,
		Namespace: repo.Namespace,
		Labels: map[string]string{
			"app.kubernetes.io/name":      repo.Name,
			"app.kubernetes.io/component": "init",
			labelManagedBy:                managedByValue,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit:            ptr.To(int32(3)),
			TTLSecondsAfterFinished: ptr.To(int32(3600)),
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{
					RestartPolicy:                corev1.RestartPolicyNever,
					AutomountServiceAccountToken: ptr.To(false),
					// The init Job only runs `restic init`, so it can afford
					// the most restrictive context there is.
					SecurityContext: &corev1.PodSecurityContext{
						RunAsNonRoot:   ptr.To(true),
						SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
					},
					Volumes: []corev1.Volume{{
						Name:     "tmp",
						EmptyDir: &corev1.EmptyDirVolumeSource{},
					}},
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
								Name: repo.SecretName(),
							},
						}},
						// restic caches under $HOME by default, which a
						// read-only root filesystem does not allow.
						Env: []corev1.EnvVar{{
							Name: "RESTIC_CACHE_DIR", Value: "/tmp/restic-cache",
						}},
						VolumeMounts: []corev1.VolumeMount{{
							Name: "tmp", MountPath: "/tmp",
						}},
						SecurityContext: &corev1.SecurityContext{
							AllowPrivilegeEscalation: ptr.To(false),
							ReadOnlyRootFilesystem:   ptr.To(true),
							Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
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

// deleteJob removes a Job and the pods it created.
func (r *RepositoryReconciler) deleteJob(ctx context.Context, job *batchv1.Job) error {
	// Background propagation so the Completed pod goes with the Job; without
	// it the pod is orphaned and lingers.
	policy := metav1.DeletePropagationBackground
	if err := r.Delete(ctx, job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
		!apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting init job %s: %w", job.Name, err)
	}
	return nil
}

// initializationRecorded reports whether Initialized is already true on the
// stored object, reading past the informer cache.
func (r *RepositoryReconciler) initializationRecorded(
	ctx context.Context, repo *borgbasev1.Repository,
) (bool, error) {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var live borgbasev1.Repository
	key := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Name}
	if err := reader.Get(ctx, key, &live); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	return live.Status.Initialized, nil
}

// cleanupInitJob removes the init Job once initialization has been recorded.
func (r *RepositoryReconciler) cleanupInitJob(
	ctx context.Context, repo *borgbasev1.Repository,
) error {
	var job batchv1.Job
	key := types.NamespacedName{Namespace: repo.Namespace, Name: repo.Name + initJobSuffix}
	if err := r.Get(ctx, key, &job); err != nil {
		return client.IgnoreNotFound(err)
	}
	return r.deleteJob(ctx, &job)
}

// jobFailedAt returns when a Job exhausted its retries, or nil if it has not.
func jobFailedAt(job *batchv1.Job) *time.Time {
	for _, c := range job.Status.Conditions {
		if c.Type == batchv1.JobFailed && c.Status == corev1.ConditionTrue {
			t := c.LastTransitionTime.Time
			return &t
		}
	}
	return nil
}

// finalize handles deletion according to the deletion policy.
//
// The BorgBase client is built only on the path that actually needs it, so a
// Retain deletion succeeds even when the API token is unavailable.
func (r *RepositoryReconciler) finalize(ctx context.Context, repo *borgbasev1.Repository) error {
	if !controllerutil.ContainsFinalizer(repo, FinalizerName) {
		return nil
	}

	switch {
	case repo.Spec.DeletionPolicy == borgbasev1.DeletionPolicyDelete && repo.Status.RepositoryID != "":
		api, err := r.apiFor(ctx, repo)
		if err != nil {
			r.setCondition(repo, metav1.Condition{
				Type:    borgbasev1.RepositoryConditionReady,
				Status:  metav1.ConditionFalse,
				Reason:  "APITokenUnavailable",
				Message: err.Error(),
			})
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "APITokenUnavailable", "Delete", "%s", err.Error())
			return err
		}
		if err := api.Delete(ctx, repo.Status.RepositoryID); err != nil && !errors.Is(err, borgbase.ErrNotFound) {
			r.Recorder.Eventf(repo, nil, corev1.EventTypeWarning, "DeleteFailed", "Delete", "%s", err.Error())
			return fmt.Errorf("deleting repository %s: %w", repo.Status.RepositoryID, err)
		}
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryDeleted", "Delete",
			"Deleted BorgBase repository %s and all its snapshots", repo.Status.RepositoryID)

	case repo.Status.RepositoryID != "":
		r.Recorder.Eventf(repo, nil, corev1.EventTypeNormal, "RepositoryRetained", "Retain",
			"Retained BorgBase repository %s; delete it manually if it is no longer needed",
			repo.Status.RepositoryID)
	}

	patch := client.MergeFrom(repo.DeepCopy())
	controllerutil.RemoveFinalizer(repo, FinalizerName)
	return r.Patch(ctx, repo, patch)
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
			Name: r.DefaultTokenSecret.Name,
			Key:  r.DefaultTokenKey,
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

// patchStatus writes the computed status as a merge patch.
//
// A full Update carries the resourceVersion read from the informer cache, which
// lags behind this controller's own writes. That produced spurious "the object
// has been modified" conflicts, and because the status never landed, the next
// reconcile re-ran initialization from scratch. A merge patch has no such
// precondition, and the whole status is recomputed each pass anyway.
func (r *RepositoryReconciler) patchStatus(
	ctx context.Context, repo *borgbasev1.Repository, base *borgbasev1.Repository,
) error {
	repo.Status.ObservedGeneration = repo.Generation
	if equality.Semantic.DeepEqual(repo.Status, base.Status) {
		return nil
	}
	return r.Status().Patch(ctx, repo, client.MergeFrom(base))
}

// SetupWithManager sets up the controller with the Manager.
func (r *RepositoryReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Recorder == nil {
		r.Recorder = mgr.GetEventRecorder("repository-controller")
	}
	if r.APIReader == nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		WithOptions(crcontroller.Options{MaxConcurrentReconciles: maxConcurrentReconciles}).
		For(&borgbasev1.Repository{}, builder.WithPredicates(ignoreOwnStatusWrites())).
		// Secrets are deliberately not owned or watched: doing so would build
		// an informer over every Secret in the cluster. A credentials Secret
		// deleted out from under us is recreated on the next periodic
		// reconcile rather than instantly.
		Owns(&batchv1.Job{}).
		Named("repository").
		Complete(r)
}

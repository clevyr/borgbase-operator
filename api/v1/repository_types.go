package v1

import (
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// DeletionPolicy controls what happens to the BorgBase repository when the
// Repository resource is deleted.
// +kubebuilder:validation:Enum=Retain;Delete
type DeletionPolicy string

const (
	// DeletionPolicyRetain leaves the BorgBase repository and the generated
	// Secret in place. This is the default: backups must never be destroyed as
	// a side effect of removing a Kubernetes object.
	DeletionPolicyRetain DeletionPolicy = "Retain"

	// DeletionPolicyDelete removes the BorgBase repository, and every snapshot
	// in it, when the Repository is deleted. This is irreversible.
	DeletionPolicyDelete DeletionPolicy = "Delete"
)

// Condition types reported on a Repository.
const (
	// RepositoryConditionReady is true once the repository exists in BorgBase,
	// the Secret has been written, and the repository has been initialized.
	RepositoryConditionReady = "Ready"

	// RepositoryConditionInitialized is true once `restic init` has succeeded
	// against the repository.
	RepositoryConditionInitialized = "Initialized"
)

// RepositorySpec defines the desired state of Repository.
//
// A Repository owns exactly one restic-format BorgBase repository plus the
// Secret holding its credentials. Set existingRepositoryID to adopt a repo that
// already exists; leave it unset to have one created.
type RepositorySpec struct {
	// interval is how often to re-reconcile against the BorgBase API, which
	// also refreshes usage and quota in the status.
	//
	// The pattern matters: the API server stores this as a plain string, and a
	// value Go cannot parse would break decoding of every Repository at once.
	// +kubebuilder:default:="1h"
	// +kubebuilder:validation:Pattern=`^([0-9]+(\.[0-9]+)?(ms|s|m|h))+$`
	// +optional
	Interval *metav1.Duration `json:"interval,omitempty"`

	// suspend pauses reconciliation of this resource.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// deletionPolicy controls whether deleting this resource also deletes the
	// BorgBase repository. Retain is strongly preferred: Delete destroys every
	// snapshot in the repository and cannot be undone.
	// +kubebuilder:default:=Retain
	// +optional
	DeletionPolicy DeletionPolicy `json:"deletionPolicy,omitempty"`

	// apiTokenSecretRef overrides the operator's default BorgBase API token.
	// Use it to point a repository at a different BorgBase account.
	// +optional
	APITokenSecretRef *corev1.SecretKeySelector `json:"apiTokenSecretRef,omitempty"`

	// existingRepositoryID adopts an already-existing BorgBase repository by
	// its opaque ID (for example "a1b2c3d4"). When set, the operator only ever
	// looks the repository up; it will never create one, and it will never
	// generate a password. This is the migration path.
	//
	// The field is immutable, and cannot be added after a repository has been
	// created for this resource: repointing a Repository at a different
	// BorgBase repo would silently orphan the original backups.
	// +kubebuilder:validation:Pattern=`^[a-z0-9]{4,32}$`
	// +optional
	ExistingRepositoryID string `json:"existingRepositoryID,omitempty"`

	// passwordSecretRef seeds the restic encryption password from an existing
	// Secret instead of generating one. Required when adopting a repository
	// that already holds snapshots, since the original password is the only
	// way to read them.
	//
	// The operator reads this Secret and never writes to it, so it remains a
	// disaster-recovery copy of the password. It must therefore be a different
	// Secret from secretName.
	// +optional
	PasswordSecretRef *corev1.SecretKeySelector `json:"passwordSecretRef,omitempty"`

	// repositoryName is the name given to a newly created BorgBase repository.
	// Defaults to the namespace of this resource. Ignored when adopting.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	RepositoryName string `json:"repositoryName,omitempty"`

	// region is the BorgBase region a new repository is created in. Ignored
	// when adopting.
	// +kubebuilder:default:="us"
	// +optional
	Region string `json:"region,omitempty"`

	// quotaGiB caps repository size. Unset means no quota. The value is passed
	// to BorgBase's quota field unchanged, and BorgBase reports sizes in GB, so
	// treat the unit as whatever BorgBase means by it. Reconciled on every
	// pass, so changing it here updates the repository.
	// +kubebuilder:validation:Minimum=1
	// +optional
	QuotaGiB *int32 `json:"quotaGiB,omitempty"`

	// alertDays makes BorgBase alert when the repository has not been written
	// to for this many days. Unset leaves the account default. Reconciled on
	// every pass.
	// +kubebuilder:validation:Minimum=0
	// +optional
	AlertDays *int32 `json:"alertDays,omitempty"`

	// appendOnly prevents clients from deleting existing data, which protects
	// backups from a compromised host. `restic forget --prune` cannot run against
	// an append-only repository. Reconciled on every pass.
	// +optional
	AppendOnly bool `json:"appendOnly,omitempty"`

	// secretName is the Secret the operator writes RESTIC_REPOSITORY and
	// RESTIC_PASSWORD into. Defaults to "<name>-borgbase". The operator only
	// writes to Secrets it created itself, so this must not name a Secret that
	// something else manages.
	// +kubebuilder:validation:MaxLength=253
	// +optional
	SecretName string `json:"secretName,omitempty"`
}

// RepositoryStatus defines the observed state of Repository.
type RepositoryStatus struct {
	// conditions represent the current state of the Repository resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// repositoryID is the BorgBase repository ID. Recording it here is the
	// point of the operator: it makes the app-to-repo mapping visible without
	// decrypting anything.
	// +optional
	RepositoryID string `json:"repositoryID,omitempty"`

	// adopted reports whether this repository was adopted rather than created.
	// +optional
	Adopted bool `json:"adopted,omitempty"`

	// initialized reports whether `restic init` has succeeded.
	// +optional
	Initialized bool `json:"initialized,omitempty"`

	// server is the BorgBase host serving this repository.
	// +optional
	Server string `json:"server,omitempty"`

	// secretName is the Secret the credentials were written to.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// currentUsage is the repository size reported by BorgBase.
	// +optional
	CurrentUsage string `json:"currentUsage,omitempty"`

	// quota is the configured size cap, if any.
	// +optional
	Quota string `json:"quota,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Repo ID",type="string",JSONPath=".status.repositoryID"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Initialized",type="boolean",JSONPath=".status.initialized"
// +kubebuilder:printcolumn:name="Usage",type="string",JSONPath=".status.currentUsage"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.existingRepositoryID) || has(self.spec.passwordSecretRef)",message="passwordSecretRef is required when adopting an existing repository, otherwise its snapshots would be unreadable"
// +kubebuilder:validation:XValidation:rule="!has(oldSelf.spec.existingRepositoryID) || (has(self.spec.existingRepositoryID) && self.spec.existingRepositoryID == oldSelf.spec.existingRepositoryID)",message="existingRepositoryID cannot be changed or removed once set"
// +kubebuilder:validation:XValidation:rule="has(oldSelf.spec.existingRepositoryID) || !has(self.spec.existingRepositoryID) || !has(oldSelf.status) || !has(oldSelf.status.repositoryID)",message="cannot adopt a different repository after one has been created for this resource"
// +kubebuilder:validation:XValidation:rule="!has(self.spec.passwordSecretRef) || !has(self.spec.secretName) || self.spec.secretName != self.spec.passwordSecretRef.name",message="secretName must not be the passwordSecretRef Secret; the operator never writes to the seed"

// Repository is the Schema for the repositories API.
type Repository struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of Repository
	// +required
	Spec RepositorySpec `json:"spec"`

	// status defines the observed state of Repository
	// +optional
	Status RepositoryStatus `json:"status,omitzero"`
}

// SecretName returns the name of the Secret the credentials are written to.
func (r *Repository) SecretName() string {
	if r.Spec.SecretName != "" {
		return r.Spec.SecretName
	}
	return r.Name + "-borgbase"
}

// RepositoryName returns the name to give a newly created BorgBase repository.
func (r *Repository) RepositoryName() string {
	if r.Spec.RepositoryName != "" {
		return r.Spec.RepositoryName
	}
	return r.Namespace
}

// +kubebuilder:object:root=true

// RepositoryList contains a list of Repository
type RepositoryList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []Repository `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &Repository{}, &RepositoryList{})
		return nil
	})
}

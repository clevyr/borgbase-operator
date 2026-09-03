package v1

import (
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
)

// SourceType selects what a backup source reads from.
// +kubebuilder:validation:Enum=cnpg;mariadb;files
type SourceType string

const (
	// SourceTypeCNPG streams a CloudNativePG dump via `dumpdb cnpg`.
	SourceTypeCNPG SourceType = "cnpg"
	// SourceTypeMariaDB streams a MariaDB dump via `dumpdb mariadb`.
	SourceTypeMariaDB SourceType = "mariadb"
	// SourceTypeFiles backs up a path on disk.
	SourceTypeFiles SourceType = "files"
)

// DatabaseEngine selects how database credentials are wired into the backup pod.
// +kubebuilder:validation:Enum=cnpg;mariadb
type DatabaseEngine string

const (
	// DatabaseEngineCNPG mounts a CloudNativePG app Secret; `dumpdb cnpg` reads
	// the connection details out of it, so no DB_* env is needed.
	DatabaseEngineCNPG DatabaseEngine = "cnpg"
	// DatabaseEngineMariaDB mounts a MariaDB Secret and additionally requires
	// DB_HOST, DB_DATABASE and DB_USERNAME in the environment.
	DatabaseEngineMariaDB DatabaseEngine = "mariadb"
)

// Condition types reported on a ScheduledBackup.
const (
	// ScheduledBackupConditionReady is true once the CronJob has been created
	// and its Repository is initialized.
	ScheduledBackupConditionReady = "Ready"
)

// BackupSource is one thing to back up. Sources render, in order, into the
// backup script as individual `restic backup` invocations.
type BackupSource struct {
	// type selects what this source reads.
	// +required
	Type SourceType `json:"type"`

	// tag is the restic tag applied to snapshots from this source. Defaults to
	// "db" for database sources and "files" for file sources.
	// +kubebuilder:validation:MaxLength=64
	// +optional
	Tag string `json:"tag,omitempty"`

	// path is the file or directory to back up, relative to the working
	// directory. Only valid when type is "files". Defaults to ".".
	// +optional
	Path string `json:"path,omitempty"`

	// exclude lists restic --exclude patterns. Only valid when type is "files".
	// +optional
	Exclude []string `json:"exclude,omitempty"`

	// database names a specific database to dump instead of the default. Only
	// valid for database sources.
	// +optional
	Database string `json:"database,omitempty"`

	// extraArgs are appended verbatim to the dump command, after a `--`
	// separator, for flags the dump tool passes through to the client.
	// +optional
	ExtraArgs []string `json:"extraArgs,omitempty"`
}

// EffectiveTag returns the restic tag for this source.
func (s BackupSource) EffectiveTag() string {
	if s.Tag != "" {
		return s.Tag
	}
	if s.Type == SourceTypeFiles {
		return "files"
	}
	return "db"
}

// EffectivePath returns the path a files source backs up.
func (s BackupSource) EffectivePath() string {
	if s.Path != "" {
		return s.Path
	}
	return "."
}

// Retention configures `restic forget --prune`. Every unset field is omitted
// from the command, so at least one must be set.
type Retention struct {
	// last keeps the N most recent snapshots regardless of age.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Last *int32 `json:"last,omitempty"`

	// hourly keeps the last N hourly snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Hourly *int32 `json:"hourly,omitempty"`

	// daily keeps the last N daily snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Daily *int32 `json:"daily,omitempty"`

	// weekly keeps the last N weekly snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Weekly *int32 `json:"weekly,omitempty"`

	// monthly keeps the last N monthly snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Monthly *int32 `json:"monthly,omitempty"`

	// yearly keeps the last N yearly snapshots.
	// +kubebuilder:validation:Minimum=1
	// +optional
	Yearly *int32 `json:"yearly,omitempty"`
}

// DatabaseSpec wires database credentials into the backup pod.
type DatabaseSpec struct {
	// engine selects the credential wiring.
	// +required
	Engine DatabaseEngine `json:"engine"`

	// secretName is the Secret mounted at /<secretName> for the dump tool to
	// read. Defaults to "postgresql-app" for cnpg and "mariadb" for mariadb.
	// +optional
	SecretName string `json:"secretName,omitempty"`

	// host sets DB_HOST. Required for mariadb, unused for cnpg.
	// +optional
	Host string `json:"host,omitempty"`

	// name sets DB_DATABASE. Required for mariadb, unused for cnpg.
	// +optional
	Name string `json:"name,omitempty"`

	// user sets DB_USERNAME. Required for mariadb, unused for cnpg.
	// +optional
	User string `json:"user,omitempty"`
}

// EffectiveSecretName returns the Secret mounted for the dump tool.
func (d *DatabaseSpec) EffectiveSecretName() string {
	if d.SecretName != "" {
		return d.SecretName
	}
	if d.Engine == DatabaseEngineMariaDB {
		return "mariadb"
	}
	return "postgresql-app"
}

// VolumeSpec attaches an existing PersistentVolumeClaim to back up files from.
type VolumeSpec struct {
	// existingClaim is the name of a PVC in this namespace.
	// +required
	ExistingClaim string `json:"existingClaim"`

	// mountPath is where the claim is mounted, which is also the working
	// directory for file sources. Defaults to "/<existingClaim>".
	// +optional
	MountPath string `json:"mountPath,omitempty"`

	// readOnly mounts the claim read-only. Off by default because RWO claims
	// shared with the app cannot always be remounted read-only.
	// +optional
	ReadOnly bool `json:"readOnly,omitempty"`
}

// EffectiveMountPath returns where the claim is mounted.
func (v *VolumeSpec) EffectiveMountPath() string {
	if v.MountPath != "" {
		return v.MountPath
	}
	return "/" + v.ExistingClaim
}

// CacheSpec configures the restic cache volume. A persistent cache makes
// `restic forget --prune` dramatically cheaper.
type CacheSpec struct {
	// enabled turns the cache PVC on. Defaults to true.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// storageClass for the cache PVC.
	// +optional
	StorageClass string `json:"storageClass,omitempty"`

	// size of the cache PVC.
	// +kubebuilder:default:="1Gi"
	// +optional
	Size string `json:"size,omitempty"`

	// accessMode for the cache PVC.
	// +kubebuilder:default:="ReadWriteMany"
	// +optional
	AccessMode corev1.PersistentVolumeAccessMode `json:"accessMode,omitempty"`
}

// HealthchecksSpec configures dead-man's-switch reporting via runitor.
//
// The operator itself never calls the healthchecks API. runitor pings
// <apiURL>/<pingKey>/<slug>?create=1, and healthchecks auto-provisions the
// check on first ping, attaching the project's notification channels.
type HealthchecksSpec struct {
	// enabled turns healthchecks reporting on for this backup. Defaults to the
	// operator's --healthchecks-enabled setting.
	// +optional
	Enabled *bool `json:"enabled,omitempty"`

	// pingKeySecretRef points at a Secret in this namespace holding the ping
	// key of the healthchecks project this backup reports to.
	//
	// There is deliberately no cluster-wide default: separate client projects
	// have separate ping keys, and a shared one would file checks into the
	// wrong project.
	// +optional
	PingKeySecretRef *corev1.SecretKeySelector `json:"pingKeySecretRef,omitempty"`

	// slug identifies the check within the project. Defaults to the namespace.
	// Must be unique within the project or pings fail with 409.
	// +kubebuilder:validation:Pattern=`^[a-z0-9_-]+$`
	// +kubebuilder:validation:MaxLength=100
	// +optional
	Slug string `json:"slug,omitempty"`

	// create auto-provisions the check on first ping. Defaults to the
	// operator's --healthchecks-auto-create setting.
	// +optional
	Create *bool `json:"create,omitempty"`

	// apiURL overrides the operator's healthchecks ping endpoint.
	// +optional
	APIURL string `json:"apiURL,omitempty"`

	// uuidSecretRef pings a check by UUID instead of by slug. This is the
	// escape hatch for adopting a check that cannot be given a slug; prefer
	// backfilling the slug so the ping path stays uniform.
	// +optional
	UUIDSecretRef *corev1.SecretKeySelector `json:"uuidSecretRef,omitempty"`
}

// ScheduledBackupSpec defines the desired state of ScheduledBackup.
type ScheduledBackupSpec struct {
	// repositoryRef names the Repository in this namespace to back up into.
	// +required
	RepositoryRef corev1.LocalObjectReference `json:"repositoryRef"`

	// schedule is either a standard five-field cron expression, used verbatim,
	// or one of the shorthands "@hourly", "@daily", "@weekly", "@monthly",
	// "@every <duration>". Shorthands are jittered from a hash of this
	// resource's identity so that copy-pasted backups do not stampede; the
	// result is reported in status.effectiveSchedule.
	// +required
	Schedule string `json:"schedule"`

	// timeZone for the schedule and for the container's TZ.
	// +kubebuilder:default:="America/Chicago"
	// +optional
	TimeZone string `json:"timeZone,omitempty"`

	// suspend pauses this backup. The CronJob is kept but suspended.
	// +optional
	Suspend bool `json:"suspend,omitempty"`

	// concurrencyPolicy for the generated CronJob.
	// +kubebuilder:default:=Forbid
	// +optional
	ConcurrencyPolicy batchv1.ConcurrencyPolicy `json:"concurrencyPolicy,omitempty"`

	// image overrides the backup image. It must provide restic, runitor, ts
	// and dumpdb. Defaults to the operator's --backup-image setting.
	// +optional
	Image string `json:"image,omitempty"`

	// sources lists what to back up. Mutually exclusive with script.
	// +optional
	Sources []BackupSource `json:"sources,omitempty"`

	// script replaces the generated backup body wholesale. The operator still
	// wraps it with runitor, timestamping and `set -eu`, and still injects the
	// environment, mounts and retention-free defaults; everything between the
	// preamble and `restic cache --cleanup` is yours. Mutually exclusive with
	// sources.
	// +optional
	Script string `json:"script,omitempty"`

	// retention configures `restic forget --prune` after the backup. Omit to
	// skip forgetting entirely.
	// +optional
	Retention *Retention `json:"retention,omitempty"`

	// env adds environment variables to the backup container. Intended as the
	// companion to script; typed fields cover the common cases.
	// +optional
	Env map[string]string `json:"env,omitempty"`

	// database wires database credentials into the backup pod.
	// +optional
	Database *DatabaseSpec `json:"database,omitempty"`

	// volume attaches an existing claim to back up files from.
	// +optional
	Volume *VolumeSpec `json:"volume,omitempty"`

	// cache configures the restic cache volume.
	// +optional
	Cache *CacheSpec `json:"cache,omitempty"`

	// healthchecks configures dead-man's-switch reporting.
	// +optional
	Healthchecks *HealthchecksSpec `json:"healthchecks,omitempty"`

	// affinity overrides the derived pod affinity. By default the backup pod is
	// scheduled next to the data it reads: hard affinity to the app when a
	// volume is attached, hard affinity to mariadb, or soft affinity to the
	// CloudNativePG primary.
	// +optional
	Affinity *corev1.Affinity `json:"affinity,omitempty"`

	// podLabels adds labels to the backup pod, for network policies and the
	// like. The mariadb-client label is added automatically for mariadb.
	// +optional
	PodLabels map[string]string `json:"podLabels,omitempty"`

	// resources for the backup container.
	// +optional
	Resources *corev1.ResourceRequirements `json:"resources,omitempty"`

	// successfulJobsHistoryLimit for the generated CronJob.
	// +kubebuilder:default:=3
	// +optional
	SuccessfulJobsHistoryLimit *int32 `json:"successfulJobsHistoryLimit,omitempty"`

	// failedJobsHistoryLimit for the generated CronJob.
	// +kubebuilder:default:=1
	// +optional
	FailedJobsHistoryLimit *int32 `json:"failedJobsHistoryLimit,omitempty"`
}

// ScheduledBackupStatus defines the observed state of ScheduledBackup.
type ScheduledBackupStatus struct {
	// conditions represent the current state of the ScheduledBackup resource.
	// +listType=map
	// +listMapKey=type
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// observedGeneration is the .metadata.generation this status was computed from.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// effectiveSchedule is the cron expression actually given to the CronJob,
	// after jitter is applied.
	// +optional
	EffectiveSchedule string `json:"effectiveSchedule,omitempty"`

	// lastScheduleTime is when the CronJob last started a backup.
	// +optional
	LastScheduleTime *metav1.Time `json:"lastScheduleTime,omitempty"`

	// lastSuccessfulTime is when a backup last completed successfully.
	// +optional
	LastSuccessfulTime *metav1.Time `json:"lastSuccessfulTime,omitempty"`

	// active is the number of currently running backup jobs.
	// +optional
	Active int32 `json:"active,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Schedule",type="string",JSONPath=".status.effectiveSchedule"
// +kubebuilder:printcolumn:name="Ready",type="string",JSONPath=".status.conditions[?(@.type=='Ready')].status"
// +kubebuilder:printcolumn:name="Last Backup",type="date",JSONPath=".status.lastSuccessfulTime"
// +kubebuilder:printcolumn:name="Suspended",type="boolean",JSONPath=".spec.suspend"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"
// +kubebuilder:validation:XValidation:rule="has(self.spec.sources) != has(self.spec.script)",message="exactly one of spec.sources or spec.script must be set"

// ScheduledBackup is the Schema for the scheduledbackups API.
type ScheduledBackup struct {
	metav1.TypeMeta `json:",inline"`

	// metadata is a standard object metadata
	// +optional
	metav1.ObjectMeta `json:"metadata,omitzero"`

	// spec defines the desired state of ScheduledBackup
	// +required
	Spec ScheduledBackupSpec `json:"spec"`

	// status defines the observed state of ScheduledBackup
	// +optional
	Status ScheduledBackupStatus `json:"status,omitzero"`
}

// Slug returns the healthchecks check slug for this backup.
func (s *ScheduledBackup) Slug() string {
	if s.Spec.Healthchecks != nil && s.Spec.Healthchecks.Slug != "" {
		return s.Spec.Healthchecks.Slug
	}
	return s.Namespace
}

// +kubebuilder:object:root=true

// ScheduledBackupList contains a list of ScheduledBackup
type ScheduledBackupList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitzero"`
	Items           []ScheduledBackup `json:"items"`
}

func init() {
	SchemeBuilder.Register(func(s *runtime.Scheme) error {
		s.AddKnownTypes(SchemeGroupVersion, &ScheduledBackup{}, &ScheduledBackupList{})
		return nil
	})
}

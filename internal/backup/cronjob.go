package backup

import (
	"fmt"
	"maps"
	"slices"
	"strconv"
	"time"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/healthchecks"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/utils/ptr"
)

// Invariants shared by every generated backup, carried over from the
// hand-written manifests this operator replaces.
const (
	// cacheMountPath is where the restic cache volume is mounted. The backup
	// image sets RESTIC_CACHE_DIR to this path.
	cacheMountPath = "/cache"

	// ttlSecondsAfterFinished cleans up finished backup Jobs after an hour,
	// which is long enough to read the logs of a failure.
	ttlSecondsAfterFinished int32 = 3600

	// cnpgDefaultCluster is the CloudNativePG Cluster name used throughout the
	// fleet, and therefore the default target for pod affinity.
	cnpgDefaultCluster = "postgresql"

	hostnameTopologyKey = "kubernetes.io/hostname"

	// Label keys and values used for pod affinity and identification.
	labelName       = "app.kubernetes.io/name"
	labelController = "app.kubernetes.io/controller"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedByValue  = "borgbase-operator"

	containerName = "restic"
	cacheVolume   = "cache"
	mariadbName   = "mariadb"
)

// Config carries operator-level settings into rendering.
type Config struct {
	// Image is the default backup image. It must provide restic, runitor, ts
	// and dumpdb.
	Image string

	// Healthchecks is the operator-level dead-man's-switch configuration.
	Healthchecks healthchecks.Config

	// CacheStorageClass is the StorageClass for restic cache volumes. It must
	// support ReadWriteMany when backups can run concurrently.
	CacheStorageClass string
}

// CacheName returns the name of the restic cache PVC for a backup.
// LabelTrigger marks how a backup Job was started. Scheduled runs carry no
// such label; only one-off runs are tagged.
const (
	LabelTrigger  = "borgbase.clevyr.com/trigger"
	TriggerManual = "manual"
)

func CacheName(sb *borgbasev1.ScheduledBackup) string { return sb.Name + "-cache" }

// CronJobName returns the name of the CronJob for a backup.
func CronJobName(sb *borgbasev1.ScheduledBackup) string { return sb.Name + "-backup" }

// maxManualJobName leaves room for the suffix the Job controller appends when
// it names pods.
const maxManualJobName = 52

// ManualJobName is the Job for a one-off run requested at the given time.
//
// It is derived from the trigger timestamp rather than randomised, so a
// reconcile that runs twice for one trigger collides on the name instead of
// starting a second backup.
func ManualJobName(sb *borgbasev1.ScheduledBackup, at time.Time) string {
	suffix := "-manual-" + strconv.FormatInt(at.Unix(), 36)
	name := sb.Name
	if limit := maxManualJobName - len(suffix); len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
}

// BuildManualJob renders a one-off run of a ScheduledBackup.
//
// It reuses the CronJob's job template verbatim, so a manual backup does
// exactly what a scheduled one does, and it is owned by the ScheduledBackup
// rather than the CronJob so the CronJob's history limits cannot delete it.
func BuildManualJob(
	sb *borgbasev1.ScheduledBackup,
	repo *borgbasev1.Repository,
	cfg Config,
	at time.Time,
) (*batchv1.Job, error) {
	tmpl, err := BuildJobTemplate(sb, repo, cfg)
	if err != nil {
		return nil, err
	}

	labels := maps.Clone(tmpl.Labels)
	if labels == nil {
		labels = map[string]string{}
	}
	labels[LabelTrigger] = TriggerManual

	return &batchv1.Job{
		Name:      ManualJobName(sb, at),
		Namespace: sb.Namespace,
		Labels:    labels,
		Spec:      tmpl.Spec,
	}, nil
}

// cacheEnabled reports whether the cache volume should be created.
func cacheEnabled(sb *borgbasev1.ScheduledBackup) bool {
	if sb.Spec.Cache == nil || sb.Spec.Cache.Enabled == nil {
		return true
	}
	return *sb.Spec.Cache.Enabled
}

// BuildCachePVC renders the restic cache volume, or nil when disabled.
//
// A persistent cache is what keeps `restic forget --prune` affordable: without
// it every run re-downloads the repository index.
func BuildCachePVC(sb *borgbasev1.ScheduledBackup, cfg Config) (*corev1.PersistentVolumeClaim, error) {
	if !cacheEnabled(sb) {
		return nil, nil
	}

	size, accessMode, storageClass := "1Gi", corev1.ReadWriteMany, cfg.CacheStorageClass
	if c := sb.Spec.Cache; c != nil {
		if c.Size != "" {
			size = c.Size
		}
		if c.AccessMode != "" {
			accessMode = c.AccessMode
		}
		if c.StorageClass != "" {
			storageClass = c.StorageClass
		}
	}

	qty, err := resource.ParseQuantity(size)
	if err != nil {
		return nil, fmt.Errorf("parsing cache size %q: %w", size, err)
	}

	pvc := &corev1.PersistentVolumeClaim{
		Name:      CacheName(sb),
		Namespace: sb.Namespace,
		Labels:    commonLabels(sb),
		Spec: corev1.PersistentVolumeClaimSpec{
			AccessModes: []corev1.PersistentVolumeAccessMode{accessMode},
			Resources: corev1.VolumeResourceRequirements{
				Requests: corev1.ResourceList{corev1.ResourceStorage: qty},
			},
		},
	}
	if storageClass != "" {
		pvc.Spec.StorageClassName = ptr.To(storageClass)
	}
	return pvc, nil
}

func commonLabels(sb *borgbasev1.ScheduledBackup) map[string]string {
	return map[string]string{
		labelName:      sb.Name,
		labelManagedBy: managedByValue,
	}
}

// BuildJobTemplate renders the Job that runs a ScheduledBackup.
//
// It is shared by the generated CronJob and by one-off runs, so a manual backup
// is byte-for-byte the same work as a scheduled one.
func BuildJobTemplate(
	sb *borgbasev1.ScheduledBackup,
	repo *borgbasev1.Repository,
	cfg Config,
) (batchv1.JobTemplateSpec, error) {
	script, err := Render(&sb.Spec)
	if err != nil {
		return batchv1.JobTemplateSpec{}, err
	}

	image := sb.Spec.Image
	if image == "" {
		image = cfg.Image
	}
	if image == "" {
		return batchv1.JobTemplateSpec{}, fmt.Errorf("no backup image configured")
	}

	env, err := buildEnv(sb, cfg)
	if err != nil {
		return batchv1.JobTemplateSpec{}, err
	}
	volumes, mounts := buildVolumes(sb)

	container := corev1.Container{
		Name:            containerName,
		Image:           image,
		Command:         buildCommand(sb, cfg, script),
		Env:             env,
		EnvFrom:         []corev1.EnvFromSource{{SecretRef: &corev1.SecretEnvSource{Name: repo.SecretName()}}},
		VolumeMounts:    mounts,
		Resources:       resources(sb),
		SecurityContext: containerSecurityContext(sb),
	}
	if sb.Spec.Volume != nil {
		// restic backs up paths relative to the working directory, so file
		// sources can be written as "." or "app" rather than absolute paths.
		container.WorkingDir = sb.Spec.Volume.EffectiveMountPath()
	}

	return batchv1.JobTemplateSpec{
		Labels: commonLabels(sb),
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To(ttlSecondsAfterFinished),
			// A backup that fails should wait for its next scheduled run rather
			// than retrying immediately against a repository that may be locked
			// by the failed attempt.
			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				Labels: podLabels(sb),
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
					Affinity:      buildAffinity(sb),
					// The backup talks to restic and the database, never to the
					// API server, so a mounted token is exposure of the
					// namespace's default ServiceAccount for nothing.
					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext:              podSecurityContext(sb),
				},
			},
		},
	}, nil
}

// BuildCronJob renders the CronJob that runs a ScheduledBackup.
func BuildCronJob(
	sb *borgbasev1.ScheduledBackup,
	repo *borgbasev1.Repository,
	cfg Config,
) (*batchv1.CronJob, error) {
	schedule, err := ResolveSchedule(sb.Spec.Schedule, sb.Namespace+"/"+sb.Name)
	if err != nil {
		return nil, err
	}

	jobTemplate, err := BuildJobTemplate(sb, repo, cfg)
	if err != nil {
		return nil, err
	}

	return &batchv1.CronJob{
		Name:      CronJobName(sb),
		Namespace: sb.Namespace,
		Labels:    commonLabels(sb),
		Spec: batchv1.CronJobSpec{
			Schedule:                   schedule,
			TimeZone:                   timeZone(sb),
			ConcurrencyPolicy:          concurrencyPolicy(sb),
			Suspend:                    ptr.To(sb.Spec.Suspend),
			SuccessfulJobsHistoryLimit: sb.Spec.SuccessfulJobsHistoryLimit,
			FailedJobsHistoryLimit:     sb.Spec.FailedJobsHistoryLimit,
			JobTemplate:                jobTemplate,
		},
	}, nil
}

func timeZone(sb *borgbasev1.ScheduledBackup) *string {
	if sb.Spec.TimeZone == "" {
		return nil
	}
	return ptr.To(sb.Spec.TimeZone)
}

func concurrencyPolicy(sb *borgbasev1.ScheduledBackup) batchv1.ConcurrencyPolicy {
	if sb.Spec.ConcurrencyPolicy != "" {
		return sb.Spec.ConcurrencyPolicy
	}
	return batchv1.ForbidConcurrent
}

func resources(sb *borgbasev1.ScheduledBackup) corev1.ResourceRequirements {
	if sb.Spec.Resources != nil {
		return *sb.Spec.Resources
	}
	return corev1.ResourceRequirements{
		Requests: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("100m"),
			corev1.ResourceMemory: resource.MustParse("256Mi"),
		},
		Limits: corev1.ResourceList{
			corev1.ResourceCPU:    resource.MustParse("1000m"),
			corev1.ResourceMemory: resource.MustParse("1Gi"),
		},
	}
}

func podLabels(sb *borgbasev1.ScheduledBackup) map[string]string {
	labels := commonLabels(sb)
	if sb.Spec.Database != nil && sb.Spec.Database.Engine == borgbasev1.DatabaseEngineMariaDB {
		// MariaDB network policies select clients by this label.
		labels["mariadb-client"] = "true"
	}
	maps.Copy(labels, sb.Spec.PodLabels)
	return labels
}

// podSecurityContext returns the pod's security context.
//
// The default sets only the seccomp profile. Pinning a user or group id is
// what makes a backup pod hard to run: the pod has to read the app's data,
// whose ownership varies per app and per cluster, and an fsGroup would chown
// that data as a side effect of backing it up. A namespace that needs more
// supplies it through spec.podSecurityContext.
func podSecurityContext(sb *borgbasev1.ScheduledBackup) *corev1.PodSecurityContext {
	if sb.Spec.PodSecurityContext != nil {
		return sb.Spec.PodSecurityContext
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// containerSecurityContext returns the backup container's security context.
//
// Capabilities and privilege escalation are locked down unconditionally, since
// restic needs neither and neither depends on which user the pod runs as. A
// read-only root filesystem is opt-in: where the image and the dump tools
// write is their business.
func containerSecurityContext(sb *borgbasev1.ScheduledBackup) *corev1.SecurityContext {
	if sb.Spec.ContainerSecurityContext != nil {
		return sb.Spec.ContainerSecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// Reporter resolves this backup's healthchecks settings, defaulting the check
// slug to the namespace. It is exported so the controller can detect two
// backups in one namespace resolving to the same check.
func Reporter(sb *borgbasev1.ScheduledBackup, cfg Config) healthchecks.Reporter {
	var o healthchecks.Overrides
	if hc := sb.Spec.Healthchecks; hc != nil {
		o = healthchecks.Overrides{
			Enabled:    hc.Enabled,
			Create:     hc.Create,
			APIURL:     hc.APIURL,
			Slug:       hc.Slug,
			PingKeyRef: hc.PingKeySecretRef,
			UUIDRef:    hc.UUIDSecretRef,
		}
	}
	return healthchecks.Resolve(cfg.Healthchecks, o, sb.Namespace)
}

// buildCommand renders the container command, wrapped for healthchecks
// reporting when it is enabled.
func buildCommand(sb *borgbasev1.ScheduledBackup, cfg Config, script string) []string {
	return Reporter(sb, cfg).Wrap([]string{"sh", "-c", script})
}

// buildEnv assembles the container environment.
func buildEnv(sb *borgbasev1.ScheduledBackup, cfg Config) ([]corev1.EnvVar, error) {
	env := []corev1.EnvVar{
		{Name: "TZ", Value: sb.Spec.TimeZone},
		{
			Name: "RESTIC_HOST",
			// Snapshots are tagged with the namespace as their host, so a
			// single repository stays legible if it ever holds more than one.
			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
	}

	if db := sb.Spec.Database; db != nil && db.Engine == borgbasev1.DatabaseEngineMariaDB {
		// dumpdb reads the password from the mounted Secret but takes the rest
		// of the connection details from the environment.
		if db.Host == "" || db.Name == "" || db.User == "" {
			return nil, fmt.Errorf("database.host, database.name and database.user are required for the mariadb engine")
		}
		env = append(env,
			corev1.EnvVar{Name: "DB_HOST", Value: db.Host},
			corev1.EnvVar{Name: "DB_DATABASE", Value: db.Name},
			corev1.EnvVar{Name: "DB_USERNAME", Value: db.User},
		)
	}

	hcEnv, err := Reporter(sb, cfg).Env()
	if err != nil {
		return nil, err
	}
	env = append(env, hcEnv...)

	// Sort user-supplied env so the rendered CronJob is stable across
	// reconciles and does not thrash on map iteration order.
	for _, k := range slices.Sorted(maps.Keys(sb.Spec.Env)) {
		env = append(env, corev1.EnvVar{Name: k, Value: sb.Spec.Env[k]})
	}
	return env, nil
}

// buildVolumes assembles the pod volumes and their mounts.
func buildVolumes(sb *borgbasev1.ScheduledBackup) ([]corev1.Volume, []corev1.VolumeMount) {
	var volumes []corev1.Volume
	var mounts []corev1.VolumeMount

	if cacheEnabled(sb) {
		volumes = append(volumes, corev1.Volume{
			Name:                  cacheVolume,
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: CacheName(sb)},
		})
		mounts = append(mounts, corev1.VolumeMount{Name: cacheVolume, MountPath: cacheMountPath})
	}

	if db := sb.Spec.Database; db != nil {
		volumes = append(volumes, corev1.Volume{
			Name:   "db-credentials",
			Secret: &corev1.SecretVolumeSource{SecretName: db.EffectiveSecretName()},
		})
		// One fixed path, not one derived from the Secret name: the rendered
		// command passes this same path as --secret-mount, so a custom
		// secretName cannot leave the dump looking somewhere else.
		mounts = append(mounts, corev1.VolumeMount{
			Name: "db-credentials", MountPath: db.MountPath(), ReadOnly: true,
		})
	}

	if v := sb.Spec.Volume; v != nil {
		volumes = append(volumes, corev1.Volume{
			Name: "data",
			PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{
				ClaimName: v.ExistingClaim,
				ReadOnly:  v.ReadOnly,
			},
		})
		mounts = append(mounts, corev1.VolumeMount{
			Name: "data", MountPath: v.EffectiveMountPath(), ReadOnly: v.ReadOnly,
		})
	}

	return volumes, mounts
}

// buildAffinity schedules the backup pod next to the data it reads.
//
// Precedence matters: a backup with an attached claim must land on the node
// holding that claim, even when it also dumps a database, because a
// ReadWriteOnce volume can only be mounted where the app already has it.
func buildAffinity(sb *borgbasev1.ScheduledBackup) *corev1.Affinity {
	if sb.Spec.Affinity != nil {
		return sb.Spec.Affinity
	}

	if v := sb.Spec.Volume; v != nil {
		return hardPodAffinity(map[string]string{
			labelName:       sb.Namespace,
			labelController: "app",
		})
	}

	db := sb.Spec.Database
	if db == nil {
		return nil
	}

	switch db.Engine {
	case borgbasev1.DatabaseEngineMariaDB:
		return hardPodAffinity(map[string]string{
			labelName:       mariadbName,
			labelController: mariadbName,
		})
	case borgbasev1.DatabaseEngineCNPG:
		// Soft, not hard: dumping across nodes is merely slower, and a hard
		// constraint would leave backups unschedulable during a failover.
		return &corev1.Affinity{
			PodAffinity: &corev1.PodAffinity{
				PreferredDuringSchedulingIgnoredDuringExecution: []corev1.WeightedPodAffinityTerm{{
					Weight: 100,
					PodAffinityTerm: corev1.PodAffinityTerm{
						TopologyKey: hostnameTopologyKey,
						LabelSelector: &metav1.LabelSelector{MatchLabels: map[string]string{
							"cnpg.io/cluster":      cnpgDefaultCluster,
							"cnpg.io/instanceRole": "primary",
						}},
					},
				}},
			},
		}
	}
	return nil
}

func hardPodAffinity(labels map[string]string) *corev1.Affinity {
	return &corev1.Affinity{
		PodAffinity: &corev1.PodAffinity{
			RequiredDuringSchedulingIgnoredDuringExecution: []corev1.PodAffinityTerm{{
				TopologyKey:   hostnameTopologyKey,
				LabelSelector: &metav1.LabelSelector{MatchLabels: labels},
			}},
		},
	}
}

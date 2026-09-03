// Package backup builds the CronJobs and Jobs that run restic.
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

const (
	cacheMountPath = "/cache"

	ttlSecondsAfterFinished int32 = 3600

	cnpgDefaultCluster = "postgresql"

	hostnameTopologyKey = "kubernetes.io/hostname"

	labelName       = "app.kubernetes.io/name"
	labelController = "app.kubernetes.io/controller"
	labelManagedBy  = "app.kubernetes.io/managed-by"
	managedByValue  = "borgbase-operator"

	containerName = "restic"
	cacheVolume   = "cache"
	mariadbName   = "mariadb"
)

// Config is the operator-wide backup configuration.
type Config struct {
	Image string

	Healthchecks healthchecks.Config

	CacheStorageClass string
}

const (
	// LabelTrigger marks how a backup Job was started.
	LabelTrigger = "borgbase.clevyr.com/trigger"
	// TriggerManual is the LabelTrigger value for a triggered backup.
	TriggerManual = "manual"
)

// CacheName returns the name of the restic cache PVC.
func CacheName(sb *borgbasev1.ScheduledBackup) string { return sb.Name + "-cache" }

// CronJobName returns the name of the backup CronJob.
func CronJobName(sb *borgbasev1.ScheduledBackup) string { return sb.Name + "-backup" }

const maxManualJobName = 52

// ManualJobName returns the Job name for a backup triggered at the given time.
func ManualJobName(sb *borgbasev1.ScheduledBackup, at time.Time) string {
	suffix := "-manual-" + strconv.FormatInt(at.Unix(), 36)
	name := sb.Name
	if limit := maxManualJobName - len(suffix); len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
}

// BuildManualJob builds a one-off backup Job.
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

func cacheEnabled(sb *borgbasev1.ScheduledBackup) bool {
	if sb.Spec.Cache == nil || sb.Spec.Cache.Enabled == nil {
		return true
	}
	return *sb.Spec.Cache.Enabled
}

// BuildCachePVC builds the restic cache claim, or nil when caching is off.
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

// BuildJobTemplate builds the pod template shared by scheduled and manual backups.
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
		container.WorkingDir = sb.Spec.Volume.EffectiveMountPath()
	}

	return batchv1.JobTemplateSpec{
		Labels: commonLabels(sb),
		Spec: batchv1.JobSpec{
			TTLSecondsAfterFinished: ptr.To(ttlSecondsAfterFinished),

			BackoffLimit: ptr.To(int32(0)),
			Template: corev1.PodTemplateSpec{
				Labels: podLabels(sb),
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers:    []corev1.Container{container},
					Volumes:       volumes,
					Affinity:      buildAffinity(sb),

					AutomountServiceAccountToken: ptr.To(false),
					SecurityContext:              podSecurityContext(sb),
				},
			},
		},
	}, nil
}

// BuildCronJob builds the CronJob that runs scheduled backups.
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
		labels["mariadb-client"] = "true"
	}
	maps.Copy(labels, sb.Spec.PodLabels)
	return labels
}

// podSecurityContext returns the pod's security context, which by default sets
// only the seccomp profile.
//
// Pinning a user or group id is what makes a backup pod hard to run: it has to
// read the app's data, whose ownership varies per app and per cluster, and an
// fsGroup would chown that data as a side effect of backing it up. A namespace
// needing more supplies it through spec.podSecurityContext.
func podSecurityContext(sb *borgbasev1.ScheduledBackup) *corev1.PodSecurityContext {
	if sb.Spec.PodSecurityContext != nil {
		return sb.Spec.PodSecurityContext
	}
	return &corev1.PodSecurityContext{
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

func containerSecurityContext(sb *borgbasev1.ScheduledBackup) *corev1.SecurityContext {
	if sb.Spec.ContainerSecurityContext != nil {
		return sb.Spec.ContainerSecurityContext
	}
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: ptr.To(false),
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
	}
}

// Reporter returns the Healthchecks reporter for a backup.
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

func buildCommand(sb *borgbasev1.ScheduledBackup, cfg Config, script string) []string {
	return Reporter(sb, cfg).Wrap([]string{"sh", "-c", script})
}

func buildEnv(sb *borgbasev1.ScheduledBackup, cfg Config) ([]corev1.EnvVar, error) {
	env := []corev1.EnvVar{
		{Name: "TZ", Value: sb.Spec.TimeZone},
		{
			Name: "RESTIC_HOST",

			ValueFrom: &corev1.EnvVarSource{
				FieldRef: &corev1.ObjectFieldSelector{FieldPath: "metadata.namespace"},
			},
		},
	}

	if db := sb.Spec.Database; db != nil && db.Engine == borgbasev1.DatabaseEngineMariaDB {
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

	for _, k := range slices.Sorted(maps.Keys(sb.Spec.Env)) {
		env = append(env, corev1.EnvVar{Name: k, Value: sb.Spec.Env[k]})
	}
	return env, nil
}

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

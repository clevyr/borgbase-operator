// Package runner runs one-off restic commands in the cluster.
//
// Everything the CLI does against a repository — listing snapshots, restoring,
// checking, an interactive shell — is the same shape: run a container that has
// restic and the repository credentials, stream it, then clean up.
package runner

import (
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/rand"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
)

const (
	// ManagedByValue marks Jobs this CLI created. It is deliberately not the
	// operator's value: the manager's Job cache and its Owns() watch are
	// filtered on that label, and a debugging pod is not a backup run.
	ManagedByValue = "corg"

	labelManagedBy = "app.kubernetes.io/managed-by"
	containerName  = "restic"
	cacheVolume    = "cache"
	dataVolume     = "data"

	// ttlSeconds is a backstop. The runner deletes its own Job on exit; this
	// only matters if the CLI is killed outright.
	ttlSeconds int32 = 600

	pollInterval = 500 * time.Millisecond
)

var (
	ErrNoCronJob = errors.New("no CronJob for this backup")
	ErrFailed    = errors.New("command failed")
	ErrNoPod     = errors.New("no pod was created")
)

// Runner creates and drives ephemeral Jobs for one ScheduledBackup.
type Runner struct {
	Client     client.Client
	Clientset  kubernetes.Interface
	RESTConfig *rest.Config
}

// Options describes one ephemeral run.
type Options struct {
	// Command replaces the backup script. Required.
	Command []string

	// Purpose names the run, e.g. "snapshots". It appears in the Job name.
	Purpose string

	// MountData attaches the backup's source volume writable, for restores.
	// It also brings back the hard pod affinity the backup uses, without which
	// a ReadWriteOnce claim cannot be mounted.
	MountData bool

	// MountCache uses the real cache PVC. Off by default: the claim may be
	// ReadWriteOnce, in which case sharing it with a running backup would make
	// this pod unschedulable.
	MountCache bool

	// Image overrides the image resolved from the CronJob.
	Image string

	// TTY requests an interactive session. The Job idles and the caller execs
	// into it instead of reading logs.
	TTY bool

	// Keep leaves the Job behind for inspection.
	Keep bool
}

// idleCommand keeps an interactive pod alive while the caller execs into it.
var idleCommand = []string{"sleep", "infinity"}

// Build renders the Job for a run.
//
// The template comes from the live CronJob rather than being re-rendered,
// because that is what the operator actually resolved: the image it defaulted
// to, the credentials Secret, the database mounts and the affinity. Re-deriving
// them here would mean re-implementing the operator's flags.
func (r *Runner) Build(
	ctx context.Context, sb *borgbasev1.ScheduledBackup, opts Options,
) (*batchv1.Job, error) {
	var cj batchv1.CronJob
	key := types.NamespacedName{Namespace: sb.Namespace, Name: backup.CronJobName(sb)}
	if err := r.Client.Get(ctx, key, &cj); err != nil {
		if apierrors.IsNotFound(err) {
			return nil, fmt.Errorf("%w: expected %s; run `corg doctor %s`",
				ErrNoCronJob, key.Name, sb.Name)
		}
		return nil, err
	}

	spec := *cj.Spec.JobTemplate.Spec.DeepCopy()
	spec.TTLSecondsAfterFinished = ptr.To(ttlSeconds)
	spec.BackoffLimit = ptr.To(int32(0))
	spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever

	container := findContainer(&spec.Template.Spec)
	if container == nil {
		return nil, fmt.Errorf("CronJob %s has no %q container", key.Name, containerName)
	}

	if opts.TTY {
		container.Command = idleCommand
		container.Stdin, container.TTY = true, true
	} else {
		container.Command = opts.Command
	}
	container.Args = nil
	if opts.Image != "" {
		container.Image = opts.Image
	}

	if !opts.MountCache {
		useEphemeralCache(&spec.Template.Spec)
	}
	if opts.MountData {
		mountDataWritable(&spec.Template.Spec, container)
	} else {
		dropData(&spec.Template.Spec, container)
	}

	labels := map[string]string{labelManagedBy: ManagedByValue}
	// MariaDB network policies select clients by this label, so a restore that
	// talks to MariaDB has to keep it.
	if v, ok := spec.Template.Labels["mariadb-client"]; ok {
		labels["mariadb-client"] = v
	}
	spec.Template.Labels = labels

	return &batchv1.Job{
		Name:      jobName(sb, opts.Purpose),
		Namespace: sb.Namespace,
		Labels:    labels,
		Spec:      spec,
	}, nil
}

func jobName(sb *borgbasev1.ScheduledBackup, purpose string) string {
	if purpose == "" {
		purpose = "run"
	}
	suffix := fmt.Sprintf("-corg-%s-%s", purpose, rand.String(5))
	name := sb.Name
	if limit := 52 - len(suffix); len(name) > limit {
		name = name[:limit]
	}
	return name + suffix
}

func findContainer(pod *corev1.PodSpec) *corev1.Container {
	for i := range pod.Containers {
		if pod.Containers[i].Name == containerName {
			return &pod.Containers[i]
		}
	}
	if len(pod.Containers) == 1 {
		return &pod.Containers[0]
	}
	return nil
}

// useEphemeralCache swaps the shared cache claim for scratch space, keeping the
// same mount path so RESTIC_CACHE_DIR still resolves.
func useEphemeralCache(pod *corev1.PodSpec) {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == cacheVolume {
			pod.Volumes[i].PersistentVolumeClaim = nil
			pod.Volumes[i].EmptyDir = &corev1.EmptyDirVolumeSource{}
		}
	}
}

func mountDataWritable(pod *corev1.PodSpec, container *corev1.Container) {
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == dataVolume && pod.Volumes[i].PersistentVolumeClaim != nil {
			pod.Volumes[i].PersistentVolumeClaim.ReadOnly = false
		}
	}
	for i := range container.VolumeMounts {
		if container.VolumeMounts[i].Name == dataVolume {
			container.VolumeMounts[i].ReadOnly = false
		}
	}
}

// dropData removes the source volume, and with it the hard pod affinity that
// exists only to place the pod where a ReadWriteOnce claim is already mounted.
// Without that, a run needing no data would be pinned to one node for nothing.
func dropData(pod *corev1.PodSpec, container *corev1.Container) {
	pod.Volumes = filterVolumes(pod.Volumes, dataVolume)
	container.VolumeMounts = filterMounts(container.VolumeMounts, dataVolume)
	if pod.Affinity != nil && pod.Affinity.PodAffinity != nil {
		pod.Affinity.PodAffinity.RequiredDuringSchedulingIgnoredDuringExecution = nil
		if pod.Affinity.PodAffinity.PreferredDuringSchedulingIgnoredDuringExecution == nil {
			pod.Affinity.PodAffinity = nil
		}
	}
}

func filterVolumes(in []corev1.Volume, name string) []corev1.Volume {
	out := make([]corev1.Volume, 0, len(in))
	for _, v := range in {
		if v.Name != name {
			out = append(out, v)
		}
	}
	return out
}

func filterMounts(in []corev1.VolumeMount, name string) []corev1.VolumeMount {
	out := make([]corev1.VolumeMount, 0, len(in))
	for _, m := range in {
		if m.Name != name {
			out = append(out, m)
		}
	}
	return out
}

// Cleanup deletes a Job and its pod.
//
// It takes its own context so that a run cancelled with Ctrl-C still tidies up:
// reusing the cancelled context would leave the Job behind.
func (r *Runner) Cleanup(job *batchv1.Job) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 30*time.Second)
	defer cancel()

	err := r.Client.Delete(ctx, job, client.PropagationPolicy(metav1.DeletePropagationBackground))
	if err != nil && !apierrors.IsNotFound(err) {
		return fmt.Errorf("deleting job/%s: %w", job.Name, err)
	}
	return nil
}

// WaitForPod blocks until the run's pod is past Pending, so there is something
// to read or attach to.
func (r *Runner) WaitForPod(ctx context.Context, job *batchv1.Job, timeout time.Duration) (*corev1.Pod, error) {
	var found *corev1.Pod

	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			var pods corev1.PodList
			err := r.Client.List(ctx, &pods,
				client.InNamespace(job.Namespace),
				client.MatchingLabels{"batch.kubernetes.io/job-name": job.Name},
			)
			if err != nil {
				return false, err
			}
			for i := range pods.Items {
				pod := &pods.Items[i]
				if pod.Status.Phase != corev1.PodPending {
					found = pod
					return true, nil
				}
				// Surface a pod that will never start rather than waiting out
				// the whole timeout on it.
				if msg := blockedReason(pod); msg != "" {
					return false, fmt.Errorf("%w: %s", ErrFailed, msg)
				}
			}
			return false, nil
		})
	if err != nil {
		return nil, err
	}
	if found == nil {
		return nil, ErrNoPod
	}
	return found, nil
}

// blockedReason reports a container state that will not resolve on its own.
func blockedReason(pod *corev1.Pod) string {
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.State.Waiting == nil {
			continue
		}
		switch cs.State.Waiting.Reason {
		case "ErrImagePull", "ImagePullBackOff", "CreateContainerConfigError", "InvalidImageName":
			return cs.State.Waiting.Reason + ": " + cs.State.Waiting.Message
		}
	}
	return ""
}

// StreamLogs copies the run's output to w, following until the pod exits.
func (r *Runner) StreamLogs(ctx context.Context, pod *corev1.Pod, w io.Writer) error {
	stream, err := r.Clientset.CoreV1().Pods(pod.Namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{Follow: true}).Stream(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = stream.Close() }()

	_, err = io.Copy(w, stream)
	return err
}

// Wait blocks until the Job finishes and reports whether it succeeded.
func (r *Runner) Wait(ctx context.Context, job *batchv1.Job, timeout time.Duration) error {
	key := client.ObjectKeyFromObject(job)

	var final batchv1.Job
	err := wait.PollUntilContextTimeout(ctx, pollInterval, timeout, true,
		func(ctx context.Context) (bool, error) {
			if err := r.Client.Get(ctx, key, &final); err != nil {
				return false, client.IgnoreNotFound(err)
			}
			return final.Status.Succeeded > 0 || final.Status.Failed > 0, nil
		})
	if err != nil {
		return err
	}
	if final.Status.Failed > 0 {
		return fmt.Errorf("%w: job/%s", ErrFailed, job.Name)
	}
	return nil
}

// Run performs a one-off command, streaming its output to w.
func (r *Runner) Run(
	ctx context.Context,
	sb *borgbasev1.ScheduledBackup,
	opts Options,
	w io.Writer,
	timeout time.Duration,
) error {
	job, err := r.Build(ctx, sb, opts)
	if err != nil {
		return err
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return fmt.Errorf("creating job/%s: %w", job.Name, err)
	}
	if !opts.Keep {
		defer func() { _ = r.Cleanup(job) }()
	}

	pod, err := r.WaitForPod(ctx, job, timeout)
	if err != nil {
		return err
	}
	if err := r.StreamLogs(ctx, pod, w); err != nil {
		return err
	}
	return r.Wait(ctx, job, timeout)
}

// Attach starts an idling pod and hands it to fn, which is expected to exec
// into it. Used for interactive sessions.
func (r *Runner) Attach(
	ctx context.Context,
	sb *borgbasev1.ScheduledBackup,
	opts Options,
	timeout time.Duration,
	fn func(pod *corev1.Pod) error,
) error {
	opts.TTY = true

	job, err := r.Build(ctx, sb, opts)
	if err != nil {
		return err
	}
	if err := r.Client.Create(ctx, job); err != nil {
		return fmt.Errorf("creating job/%s: %w", job.Name, err)
	}
	if !opts.Keep {
		defer func() { _ = r.Cleanup(job) }()
	}

	pod, err := r.WaitForPod(ctx, job, timeout)
	if err != nil {
		return err
	}
	return fn(pod)
}

// ContainerName is the container an interactive session should attach to.
const ContainerName = containerName

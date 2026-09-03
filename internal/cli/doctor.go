package cli

import (
	"context"
	"errors"
	"fmt"
	"io"

	"github.com/spf13/cobra"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"github.com/clevyr/borgbase-operator/internal/controller"
)

// ErrUnhealthy is returned when any check fails, so doctor is usable in scripts.
var ErrUnhealthy = errors.New("one or more checks failed")

type checkLevel int

const (
	levelOK checkLevel = iota
	levelInfo
	levelWarn
	levelFail
)

func (l checkLevel) symbol() string {
	switch l {
	case levelOK:
		return "✔"
	case levelInfo:
		return "·"
	case levelWarn:
		return "!"
	default:
		return "✖"
	}
}

type check struct {
	level   checkLevel
	summary string
	detail  []string
}

type report struct {
	subject string
	checks  []check
}

func (r *report) add(level checkLevel, summary string, detail ...string) {
	r.checks = append(r.checks, check{level: level, summary: summary, detail: detail})
}

func (r *report) failed() bool {
	for _, c := range r.checks {
		if c.level == levelFail {
			return true
		}
	}
	return false
}

func (r *report) write(p *printer) {
	p.println(r.subject)
	for _, c := range r.checks {
		p.printf("  %s %s\n", c.level.symbol(), c.summary)
		for _, d := range c.detail {
			p.printf("      %s\n", d)
		}
	}
}

func newDoctorCommand(f *Factory) *cobra.Command {
	cmd := &cobra.Command{
		Use:     "doctor [name]",
		Short:   "Explain why a backup is not running",
		GroupID: GroupInspect,
		Long: `Diagnose repositories and scheduled backups.

Checks the things that actually break: a missing or unreadable credentials
Secret, a CronJob owned by something else, a repository that never initialized,
and a most-recent run that did not succeed.

With no argument every resource in the namespace is checked. Exits non-zero if
any check fails.`,
		Args: cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := f.Client()
			if err != nil {
				return err
			}
			ns, err := f.Namespace()
			if err != nil {
				return err
			}

			var arg string
			if len(args) == 1 {
				arg = args[0]
			}
			return runDoctor(cmd.Context(), c, f.Streams.Out, ns, arg)
		},
	}
	return cmd
}

func runDoctor(ctx context.Context, c client.Client, out io.Writer, namespace, arg string) error {
	reports, err := collectReports(ctx, c, namespace, arg)
	if err != nil {
		return err
	}

	p := newPrinter(out)

	if len(reports) == 0 {
		p.printf("No resources found in namespace %q.\n", namespace)
		return p.Err()
	}

	unhealthy := false
	for i, r := range reports {
		if i > 0 {
			p.println()
		}
		r.write(p)
		unhealthy = unhealthy || r.failed()
	}

	if err := p.Err(); err != nil {
		return err
	}
	if unhealthy {
		return ErrUnhealthy
	}
	return nil
}

func collectReports(ctx context.Context, c client.Client, namespace, arg string) ([]*report, error) {
	if arg != "" {
		target, err := Resolve(ctx, c, namespace, arg)
		if err != nil {
			return nil, err
		}
		if target.Kind == TargetRepository {
			return []*report{checkRepository(ctx, c, target.Repository)}, nil
		}
		return []*report{checkScheduledBackup(ctx, c, target.ScheduledBackup)}, nil
	}

	var repos borgbasev1.RepositoryList
	if err := c.List(ctx, &repos, client.InNamespace(namespace)); err != nil {
		return nil, err
	}
	var backups borgbasev1.ScheduledBackupList
	if err := c.List(ctx, &backups, client.InNamespace(namespace)); err != nil {
		return nil, err
	}

	reports := make([]*report, 0, len(repos.Items)+len(backups.Items))
	for i := range repos.Items {
		reports = append(reports, checkRepository(ctx, c, &repos.Items[i]))
	}
	for i := range backups.Items {
		reports = append(reports, checkScheduledBackup(ctx, c, &backups.Items[i]))
	}
	return reports, nil
}

func checkRepository(ctx context.Context, c client.Client, repo *borgbasev1.Repository) *report {
	r := &report{subject: "repository/" + repo.Name}

	if repo.Spec.Suspend {
		r.add(levelWarn, "reconciliation is suspended",
			"The operator will not touch this repository until spec.suspend is cleared.")
		return r
	}

	checkGeneration(r, repo.Generation, repo.Status.ObservedGeneration)
	checkCondition(r, repo.Status.Conditions, borgbasev1.RepositoryConditionReady, "repository")

	switch {
	case repo.Status.Initialized:
		r.add(levelOK, "restic repository is initialized")
	default:
		r.add(levelFail, "restic repository is not initialized")
		describeInitJobs(ctx, c, repo, r)
	}

	if repo.Status.RepositoryID == "" {
		r.add(levelWarn, "no BorgBase repository ID recorded yet")
	} else if repo.Status.Adopted {
		r.add(levelInfo, fmt.Sprintf("adopted existing BorgBase repository %s", repo.Status.RepositoryID))
	}

	checkCredentialsSecret(ctx, c, repo, r)
	return r
}

func checkScheduledBackup(ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup) *report {
	r := &report{subject: "scheduledbackup/" + sb.Name}

	if sb.Spec.Suspend {
		r.add(levelWarn, "backups are suspended",
			"The CronJob is kept but will not fire. Clear spec.suspend to resume.")
	}

	checkGeneration(r, sb.Generation, sb.Status.ObservedGeneration)
	checkCondition(r, sb.Status.Conditions, borgbasev1.ScheduledBackupConditionReady, "backup")

	repo, err := RepositoryFor(ctx, c, sb)
	switch {
	case errors.Is(err, ErrTargetNotFound):
		r.add(levelFail, fmt.Sprintf("repository %q does not exist", sb.Spec.RepositoryRef.Name),
			"spec.repositoryRef must name a Repository in this namespace.")
	case err != nil:
		r.add(levelFail, "could not read the referenced repository", err.Error())
	default:
		checkReferencedRepository(ctx, c, repo, r)
	}

	checkCronJob(ctx, c, sb, r)
	checkCachePVC(ctx, c, sb, r)
	checkLastRun(sb, r)
	return r
}

func checkReferencedRepository(ctx context.Context, c client.Client, repo *borgbasev1.Repository, r *report) {
	ready := FindCondition(repo.Status.Conditions, borgbasev1.RepositoryConditionReady)
	switch {
	case !repo.Status.Initialized:
		r.add(levelFail, fmt.Sprintf("repository %q is not initialized", repo.Name),
			"Backups cannot run until `restic init` succeeds. Run: corg doctor repo/"+repo.Name)
	case ready == nil || ready.Status != metav1.ConditionTrue:
		r.add(levelWarn, fmt.Sprintf("repository %q is not ready", repo.Name),
			"Run: corg doctor repo/"+repo.Name)
	default:
		r.add(levelOK, fmt.Sprintf("repository %q is ready and initialized", repo.Name))
	}

	checkCredentialsSecret(ctx, c, repo, r)
}

// checkCredentialsSecret verifies the Secret the backup pod loads with envFrom.
// A missing key produces a container that starts and then fails to reach the
// repository, which is a confusing failure to debug from logs alone.
func checkCredentialsSecret(ctx context.Context, c client.Client, repo *borgbasev1.Repository, r *report) {
	name := repo.SecretName()

	var secret corev1.Secret
	err := c.Get(ctx, types.NamespacedName{Namespace: repo.Namespace, Name: name}, &secret)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelFail, fmt.Sprintf("credentials Secret %q does not exist", name),
			"The operator writes it on reconcile; if it is missing the repository has not reconciled.")
		return
	case err != nil:
		r.add(levelFail, fmt.Sprintf("could not read Secret %q", name), err.Error())
		return
	}

	var missing []string
	for _, key := range []string{controller.KeyResticRepository, controller.KeyResticPassword} {
		if len(secret.Data[key]) == 0 {
			missing = append(missing, key)
		}
	}
	if len(missing) > 0 {
		r.add(levelFail, fmt.Sprintf("Secret %q is missing %v", name, missing))
		return
	}
	r.add(levelOK, fmt.Sprintf("credentials Secret %q has both keys", name))
}

// checkCronJob catches the migration failure the operator refuses to work
// around: a CronJob of the same name owned by something else, usually a
// leftover Flux HelmRelease.
func checkCronJob(ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup, r *report) {
	name := backup.CronJobName(sb)

	var cj batchv1.CronJob
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, &cj)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelFail, fmt.Sprintf("CronJob %q does not exist", name),
			"The operator creates it once the backup is ready.")
		return
	case err != nil:
		r.add(levelFail, fmt.Sprintf("could not read CronJob %q", name), err.Error())
		return
	}

	if !metav1.IsControlledBy(&cj, sb) {
		detail := []string{"The operator will not adopt a CronJob it does not own."}
		if owner := metav1.GetControllerOf(&cj); owner != nil {
			detail = append(detail, fmt.Sprintf("It is controlled by %s/%s.", owner.Kind, owner.Name))
		}
		detail = append(detail, "Remove whatever created it, then delete cronjob/"+name+".")
		r.add(levelFail, fmt.Sprintf("CronJob %q is not controlled by this ScheduledBackup", name), detail...)
		return
	}

	r.add(levelOK, fmt.Sprintf("CronJob %q is owned by this ScheduledBackup", name))

	if suspended := cj.Spec.Suspend != nil && *cj.Spec.Suspend; suspended != sb.Spec.Suspend {
		r.add(levelWarn, "CronJob suspend state does not match spec.suspend",
			fmt.Sprintf("CronJob suspended=%t, spec.suspend=%t. The operator has not reconciled yet.",
				suspended, sb.Spec.Suspend))
	}
}

func checkCachePVC(ctx context.Context, c client.Client, sb *borgbasev1.ScheduledBackup, r *report) {
	if sb.Spec.Cache != nil && sb.Spec.Cache.Enabled != nil && !*sb.Spec.Cache.Enabled {
		return
	}
	name := backup.CacheName(sb)

	var pvc corev1.PersistentVolumeClaim
	err := c.Get(ctx, types.NamespacedName{Namespace: sb.Namespace, Name: name}, &pvc)
	switch {
	case apierrors.IsNotFound(err):
		r.add(levelWarn, fmt.Sprintf("cache PVC %q does not exist", name),
			"Backups still run, but restic re-reads the repository index every time.")
	case err != nil:
		r.add(levelWarn, fmt.Sprintf("could not read PVC %q", name), err.Error())
	case pvc.Status.Phase != corev1.ClaimBound:
		r.add(levelWarn, fmt.Sprintf("cache PVC %q is %s, not Bound", name, pvc.Status.Phase),
			"A pending claim will block the backup pod from starting.")
	default:
		r.add(levelOK, fmt.Sprintf("cache PVC %q is bound", name))
	}
}

// checkLastRun compares the last scheduled start against the last success, so a
// run that started and failed is reported as a failure rather than as silence.
func checkLastRun(sb *borgbasev1.ScheduledBackup, r *report) {
	st := sb.Status

	if st.Active > 0 {
		r.add(levelInfo, fmt.Sprintf("%d backup(s) running now", st.Active))
		return
	}

	switch {
	case st.LastScheduleTime == nil:
		r.add(levelWarn, "no backup has been scheduled yet")
	case st.LastSuccessfulTime == nil:
		r.add(levelFail, fmt.Sprintf("a backup started %s but none has ever succeeded", Since(st.LastScheduleTime)),
			"Run: corg logs "+sb.Name)
	case st.LastSuccessfulTime.Before(st.LastScheduleTime):
		r.add(levelFail, fmt.Sprintf("the most recent run (%s) did not succeed", Since(st.LastScheduleTime)),
			fmt.Sprintf("Last success was %s.", Since(st.LastSuccessfulTime)),
			"Run: corg logs "+sb.Name)
	default:
		r.add(levelOK, fmt.Sprintf("last backup succeeded %s", Since(st.LastSuccessfulTime)))
	}
}

func checkGeneration(r *report, generation, observed int64) {
	if observed != 0 && observed < generation {
		r.add(levelWarn, "the operator has not caught up with the latest spec change",
			fmt.Sprintf("generation %d, observed %d.", generation, observed))
	}
}

func checkCondition(r *report, conds []metav1.Condition, condType, noun string) {
	cond := FindCondition(conds, condType)
	switch {
	case cond == nil:
		r.add(levelWarn, "the operator has not reconciled this "+noun+" yet")
	case cond.Status == metav1.ConditionTrue:
		r.add(levelOK, noun+" is ready")
	default:
		detail := []string{}
		if cond.Message != "" {
			detail = append(detail, cond.Message)
		}
		r.add(levelFail, fmt.Sprintf("%s is not ready (%s)", noun, cond.Reason), detail...)
	}
}

// describeInitJobs reports why initialization has not finished, using the Jobs
// the Repository owns rather than guessing their names.
func describeInitJobs(ctx context.Context, c client.Client, repo *borgbasev1.Repository, r *report) {
	var jobs batchv1.JobList
	if err := c.List(ctx, &jobs, client.InNamespace(repo.Namespace)); err != nil {
		return
	}

	for i := range jobs.Items {
		job := &jobs.Items[i]
		if !metav1.IsControlledBy(job, repo) {
			continue
		}
		switch {
		case job.Status.Failed > 0:
			detail := []string{"Run: kubectl logs job/" + job.Name}
			for _, cond := range job.Status.Conditions {
				if cond.Type == batchv1.JobFailed && cond.Message != "" {
					detail = append([]string{cond.Message}, detail...)
				}
			}
			r.add(levelInfo, fmt.Sprintf("init Job %q has failed %d time(s)", job.Name, job.Status.Failed), detail...)
		case job.Status.Active > 0:
			r.add(levelInfo, fmt.Sprintf("init Job %q is running", job.Name))
		}
		return
	}

	r.add(levelInfo, "no init Job exists yet")
}

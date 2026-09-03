package controller

import (
	"context"
	"testing"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/events"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/borgbase"
)

const (
	testNS     = "myapp-prod"
	resticName = "restic"
	seedSecret = "restic-envs"
	tokenNS    = "borgbase-system"
	tokenName  = "borgbase-api"
)

func testScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	s := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	if err := borgbasev1.AddToScheme(s); err != nil {
		t.Fatal(err)
	}
	return s
}

// newHarness wires a reconciler against a fake cluster and a fake BorgBase.
func newHarness(t *testing.T, api *fakeAPI, objs ...client.Object) (*RepositoryReconciler, client.Client) {
	t.Helper()
	scheme := testScheme(t)

	all := append([]client.Object{&corev1.Secret{
		Name: tokenName, Namespace: tokenNS,
		Data: map[string][]byte{"token": []byte("test-token")},
	}}, objs...)

	c := fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(all...).
		WithStatusSubresource(&borgbasev1.Repository{}).
		Build()

	return &RepositoryReconciler{
		Client:             c,
		Scheme:             scheme,
		Recorder:           events.NewFakeRecorder(100),
		NewAPI:             func(string) borgbase.API { return api },
		DefaultTokenSecret: types.NamespacedName{Namespace: tokenNS, Name: tokenName},
		DefaultTokenKey:    "token",
		BackupImage:        "ghcr.io/clevyr/restic:test",
	}, c
}

func repositoryFixture(mutate func(*borgbasev1.Repository)) *borgbasev1.Repository {
	repo := &borgbasev1.Repository{
		Name: resticName, Namespace: testNS,
		Spec: borgbasev1.RepositorySpec{
			Region:         "us",
			DeletionPolicy: borgbasev1.DeletionPolicyRetain,
		},
	}
	if mutate != nil {
		mutate(repo)
	}
	return repo
}

func reconcileN(t *testing.T, r *RepositoryReconciler, n int) {
	t.Helper()
	req := ctrl.Request{Namespace: testNS, Name: resticName}
	for i := range n {
		if _, err := r.Reconcile(context.Background(), req); err != nil {
			t.Fatalf("reconcile %d: %v", i, err)
		}
	}
}

func getSecret(t *testing.T, c client.Client, name string) *corev1.Secret {
	t.Helper()
	var s corev1.Secret
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &s); err != nil {
		t.Fatalf("getting secret %s: %v", name, err)
	}
	return &s
}

// A generated password is the encryption key for every snapshot. Once written
// it must survive any number of reconciles unchanged; regenerating it would
// silently orphan every existing backup.
func TestPasswordIsGeneratedOnceAndNeverRotated(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))

	reconcileN(t, r, 1)
	first := string(getSecret(t, c, "restic-borgbase").Data[KeyResticPassword])
	if first == "" {
		t.Fatal("no password was written")
	}

	reconcileN(t, r, 5)
	after := string(getSecret(t, c, "restic-borgbase").Data[KeyResticPassword])
	if after != first {
		t.Errorf("password changed across reconciles:\n first = %q\n after = %q", first, after)
	}
}

// Adoption must never fall back to creating a repository: a mistyped ID that
// silently provisioned an empty repo would look healthy while backing up
// nothing, and the real backups would stop being written to.
func TestAdoptionNeverCreates(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: "a1b2c3d4", Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "secret-token", CurrentUsage: 3.2,
	})
	repo := repositoryFixture(func(r *borgbasev1.Repository) {
		r.Spec.ExistingRepositoryID = "a1b2c3d4"
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{
			Name: seedSecret,
			Key:  KeyResticPassword,
		}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("existing-password-from-sops")},
	}

	r, c := newHarness(t, api, repo, seed)
	reconcileN(t, r, 3)

	if api.called("Add") {
		t.Error("adoption called repoAdd; it must only ever look the repository up")
	}

	secret := getSecret(t, c, "restic-borgbase")
	if got := string(secret.Data[KeyResticPassword]); got != "existing-password-from-sops" {
		t.Errorf("adopted password = %q, want the seeded one", got)
	}
	want := "rest:https://a1b2c3d4:secret-token@a1b2c3d4.repo.borgbase.com"
	if got := string(secret.Data[KeyResticRepository]); got != want {
		t.Errorf("RESTIC_REPOSITORY = %q, want %q", got, want)
	}

	// The seed Secret is the only off-cluster copy of the password, so the
	// operator must leave it exactly as it found it.
	var afterSeed corev1.Secret
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: seedSecret}, &afterSeed); err != nil {
		t.Fatal(err)
	}
	if string(afterSeed.Data[KeyResticPassword]) != "existing-password-from-sops" {
		t.Error("the operator modified the seed secret")
	}
}

// A repository that already holds data, but whose password we do not have,
// must fail loudly rather than being handed a fresh password that cannot
// decrypt anything already in it.
func TestRefusesToInventPasswordForNonEmptyRepository(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: "abc12345", Name: testNS, Format: borgbase.FormatRestic,
		Htpasswd: "t", CurrentUsage: 12.5,
	})
	repo := repositoryFixture(func(r *borgbasev1.Repository) {
		r.Status.RepositoryID = "abc12345"
	})

	r, _ := newHarness(t, api, repo)
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("expected an error for a non-empty repository with no password")
	}
}

// A non-restic repository can never be corrected, because BorgBase fixes the
// format at creation. It must be a hard failure, not a silently broken URL.
func TestRejectsNonResticRepository(t *testing.T) {
	api := newFakeAPI(&borgbase.Repo{
		ID: "borgrepo", Name: "myapp-prod", Format: "borg", Htpasswd: "t",
	})
	repo := repositoryFixture(func(r *borgbasev1.Repository) {
		r.Spec.ExistingRepositoryID = "borgrepo"
		r.Spec.PasswordSecretRef = &corev1.SecretKeySelector{
			Name: seedSecret,
			Key:  KeyResticPassword,
		}
	})
	seed := &corev1.Secret{
		Name: seedSecret, Namespace: testNS,
		Data: map[string][]byte{KeyResticPassword: []byte("pw")},
	}

	r, _ := newHarness(t, api, repo, seed)
	_, err := r.Reconcile(context.Background(),
		ctrl.Request{Namespace: testNS, Name: resticName})
	if err == nil {
		t.Fatal("expected an error for a non-restic repository")
	}
}

func TestCreatesRepositoryAndInitJob(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	if !api.called("Add") {
		t.Error("expected a repository to be created")
	}

	var job batchv1.Job
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: "restic-init"}, &job); err != nil {
		t.Fatalf("expected an init job: %v", err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	// Probing before initializing keeps genuine failures visible, unlike the
	// `restic init || true` this replaces.
	want := "restic cat config >/dev/null 2>&1 || restic init"
	if got := container.Command[len(container.Command)-1]; got != want {
		t.Errorf("init command = %q, want %q", got, want)
	}
	if container.EnvFrom[0].SecretRef.Name != "restic-borgbase" {
		t.Errorf("init job reads the wrong secret: %s", container.EnvFrom[0].SecretRef.Name)
	}
	// The Job never calls the API server, so a mounted token is needless
	// exposure of the namespace's default ServiceAccount.
	if automount := job.Spec.Template.Spec.AutomountServiceAccountToken; automount == nil || *automount {
		t.Error("init job should not mount a service account token")
	}
	// Requests keep the pod out of BestEffort, which would make it the first
	// thing evicted and leave the repository stuck uninitialized.
	if container.Resources.Requests.Cpu().IsZero() || container.Resources.Requests.Memory().IsZero() {
		t.Error("init job should set resource requests")
	}
}

// A finished init Job should not linger as a Completed pod waiting for its TTL.
// It is removed on the pass after initialization is recorded, not the same one:
// deleting it immediately fires a watch event that can be handled before the
// status write lands, and that reconcile would start a second Job.
func TestSucceededInitJobIsRemovedOnTheNextPass(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	key := types.NamespacedName{Namespace: testNS, Name: "restic-init"}
	var job batchv1.Job
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Fatalf("expected an init job: %v", err)
	}

	// Mark it succeeded, as the Job controller would.
	job.Status.Succeeded = 1
	if err := c.Status().Update(context.Background(), &job); err != nil {
		t.Fatal(err)
	}

	// First pass records initialization and leaves the Job alone.
	reconcileN(t, r, 1)
	var repo borgbasev1.Repository
	if err := c.Get(context.Background(),
		types.NamespacedName{Namespace: testNS, Name: resticName}, &repo); err != nil {
		t.Fatal(err)
	}
	if !repo.Status.Initialized {
		t.Fatal("repository should be marked initialized")
	}
	if err := c.Get(context.Background(), key, &job); err != nil {
		t.Errorf("init job should survive the pass that records success: %v", err)
	}

	// The next pass, with Initialized already persisted, removes it.
	reconcileN(t, r, 1)
	if err := c.Get(context.Background(), key, &job); !apierrors.IsNotFound(err) {
		t.Errorf("init job should have been deleted, got %v", err)
	}

	// And no replacement is ever created.
	reconcileN(t, r, 3)
	if err := c.Get(context.Background(), key, &job); !apierrors.IsNotFound(err) {
		t.Error("a second init job was created after initialization completed")
	}
}

// Retain is the default precisely so that removing a Kubernetes object can
// never destroy backups.
func TestDeletionPolicyRetainKeepsRepositoryAndSecret(t *testing.T) {
	api := newFakeAPI()
	r, c := newHarness(t, api, repositoryFixture(nil))
	reconcileN(t, r, 2)

	var repo borgbasev1.Repository
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &repo); err != nil {
		t.Fatal(err)
	}
	id := repo.Status.RepositoryID
	if id == "" {
		t.Fatal("no repository id recorded")
	}
	if err := c.Delete(context.Background(), &repo); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if api.called("Delete") {
		t.Error("Retain called repoDelete; backups must survive deleting the resource")
	}
	if _, ok := api.repos[id]; !ok {
		t.Error("the BorgBase repository was removed under the Retain policy")
	}
	// The Secret holds the only copy of the encryption key, so it must not be
	// garbage collected with the resource.
	if _, err := getSecretOrErr(c, "restic-borgbase"); err != nil {
		t.Errorf("credentials secret was removed under the Retain policy: %v", err)
	}
	if err := c.Get(context.Background(), key, &repo); !apierrors.IsNotFound(err) {
		t.Errorf("finalizer was not removed, repository still present: %v", err)
	}
}

func TestDeletionPolicyDeleteRemovesRepository(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *borgbasev1.Repository) {
		r.Spec.DeletionPolicy = borgbasev1.DeletionPolicyDelete
	})
	r, c := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	var got borgbasev1.Repository
	key := types.NamespacedName{Namespace: testNS, Name: resticName}
	if err := c.Get(context.Background(), key, &got); err != nil {
		t.Fatal(err)
	}
	id := got.Status.RepositoryID
	if err := c.Delete(context.Background(), &got); err != nil {
		t.Fatal(err)
	}
	reconcileN(t, r, 1)

	if !api.called("Delete") {
		t.Error("expected repoDelete under the Delete policy")
	}
	if _, ok := api.repos[id]; ok {
		t.Error("the BorgBase repository survived the Delete policy")
	}
}

// Suspending must stop the controller touching BorgBase at all, so it can be
// used to freeze a repository while investigating a problem.
func TestSuspendSkipsReconcile(t *testing.T) {
	api := newFakeAPI()
	repo := repositoryFixture(func(r *borgbasev1.Repository) { r.Spec.Suspend = true })
	r, _ := newHarness(t, api, repo)
	reconcileN(t, r, 2)

	if api.called("Add") || api.called("FindByName") {
		t.Errorf("suspended repository still contacted BorgBase: %v", api.calls)
	}
}

func getSecretOrErr(c client.Client, name string) (*corev1.Secret, error) {
	var s corev1.Secret
	err := c.Get(context.Background(), types.NamespacedName{Namespace: testNS, Name: name}, &s)
	return &s, err
}

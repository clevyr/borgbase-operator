package cli

import (
	"sync"

	"github.com/spf13/cobra"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/cli-runtime/pkg/genericclioptions"
	"k8s.io/cli-runtime/pkg/genericiooptions"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"sigs.k8s.io/controller-runtime/pkg/client"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
)

// Factory lazily builds the clients a command needs from the standard kubectl
// connection flags, so no command pays for a connection it does not use.
type Factory struct {
	ConfigFlags *genericclioptions.ConfigFlags
	Streams     genericiooptions.IOStreams

	// allNamespaces is bound to -A by the root command.
	allNamespaces bool

	once struct {
		scheme     sync.Once
		restConfig sync.Once
		client     sync.Once
		clientset  sync.Once
	}
	scheme     *runtime.Scheme
	restConfig *rest.Config
	restErr    error
	client     client.Client
	clientErr  error
	clientset  *kubernetes.Clientset
	csErr      error
}

func NewFactory(streams genericiooptions.IOStreams) *Factory {
	return &Factory{
		// usePersistentConfig keeps the discovery cache warm across calls.
		ConfigFlags: genericclioptions.NewConfigFlags(true),
		Streams:     streams,
	}
}

// Scheme carries the operator's own types alongside the core and batch types
// the CLI reads directly.
func (f *Factory) Scheme() *runtime.Scheme {
	f.once.scheme.Do(func() {
		s := runtime.NewScheme()
		utilruntime.Must(corev1.AddToScheme(s))
		utilruntime.Must(batchv1.AddToScheme(s))
		utilruntime.Must(borgbasev1.AddToScheme(s))
		f.scheme = s
	})
	return f.scheme
}

func (f *Factory) RESTConfig() (*rest.Config, error) {
	f.once.restConfig.Do(func() {
		f.restConfig, f.restErr = f.ConfigFlags.ToRESTConfig()
	})
	return f.restConfig, f.restErr
}

// Client returns a typed client for the operator's CRDs and the objects it owns.
func (f *Factory) Client() (client.Client, error) {
	f.once.client.Do(func() {
		cfg, err := f.RESTConfig()
		if err != nil {
			f.clientErr = err
			return
		}
		f.client, f.clientErr = client.New(cfg, client.Options{Scheme: f.Scheme()})
	})
	return f.client, f.clientErr
}

// Clientset is needed for the subresources controller-runtime does not expose:
// pod logs and exec.
func (f *Factory) Clientset() (*kubernetes.Clientset, error) {
	f.once.clientset.Do(func() {
		cfg, err := f.RESTConfig()
		if err != nil {
			f.csErr = err
			return
		}
		f.clientset, f.csErr = kubernetes.NewForConfig(cfg)
	})
	return f.clientset, f.csErr
}

// Namespace resolves -n, falling back to the kubeconfig context's namespace.
func (f *Factory) Namespace() (string, error) {
	ns, _, err := f.ConfigFlags.ToRawKubeConfigLoader().Namespace()
	return ns, err
}

// AllNamespaces reports whether -A was passed.
func (f *Factory) AllNamespaces() bool { return f.allNamespaces }

// AddAllNamespacesFlag registers -A on the commands that can honour it, rather
// than on the root, so a flag never appears where it would be meaningless.
func (f *Factory) AddAllNamespacesFlag(cmd *cobra.Command) {
	cmd.Flags().BoolVarP(&f.allNamespaces, "all-namespaces", "A", false,
		"List the requested objects across all namespaces")
}

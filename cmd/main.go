package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"github.com/clevyr/borgbase-operator/internal/controller"
	"github.com/clevyr/borgbase-operator/internal/healthchecks"
	// +kubebuilder:scaffold:imports
)

const defaultBackupImage = "ghcr.io/clevyr/restic:0.18.1"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

func parseNamespacedName(s, defaultNS string) (types.NamespacedName, error) {
	ns, name, ok := strings.Cut(s, "/")
	if !ok {
		if defaultNS == "" {
			return types.NamespacedName{}, fmt.Errorf(
				"%q has no namespace and POD_NAMESPACE is unset; use namespace/name", s)
		}
		return types.NamespacedName{Namespace: defaultNS, Name: s}, nil
	}
	if ns == "" || name == "" {
		return types.NamespacedName{}, fmt.Errorf("expected namespace/name, got %q", s)
	}
	return types.NamespacedName{Namespace: ns, Name: name}, nil
}

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(borgbasev1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	var apiTokenSecret, apiTokenKey, backupImage, cacheStorageClass, borgbaseEndpoint string
	var healthchecksEnabled, healthchecksAutoCreate bool
	var healthchecksAPIURL string
	flag.StringVar(&metricsAddr, "metrics-bind-address", "0", "The address the metrics endpoint binds to. "+
		"Use :8443 for HTTPS or :8080 for HTTP, or leave as 0 to disable the metrics service.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.BoolVar(&secureMetrics, "metrics-secure", true,
		"If set, the metrics endpoint is served securely via HTTPS. Use --metrics-secure=false to use HTTP instead.")
	flag.StringVar(&webhookCertPath, "webhook-cert-path", "", "The directory that contains the webhook certificate.")
	flag.StringVar(&webhookCertName, "webhook-cert-name", "tls.crt", "The name of the webhook certificate file.")
	flag.StringVar(&webhookCertKey, "webhook-cert-key", "tls.key", "The name of the webhook key file.")
	flag.StringVar(&metricsCertPath, "metrics-cert-path", "",
		"The directory that contains the metrics server certificate.")
	flag.StringVar(&metricsCertName, "metrics-cert-name", "tls.crt", "The name of the metrics server certificate file.")
	flag.StringVar(&metricsCertKey, "metrics-cert-key", "tls.key", "The name of the metrics server key file.")
	flag.BoolVar(&enableHTTP2, "enable-http2", false,
		"If set, HTTP/2 will be enabled for the metrics and webhook servers")

	opts := zap.Options{
		Development: false,
	}
	flag.StringVar(&apiTokenSecret, "api-token-secret", "borgbase-api",
		"Secret holding the default BorgBase API token, as \"name\" or "+
			"\"namespace/name\". A bare name resolves in the operator's own namespace. "+
			"A Repository may override this with spec.apiTokenSecretRef.")
	flag.StringVar(&borgbaseEndpoint, "borgbase-endpoint", "",
		"Override the BorgBase GraphQL endpoint. Empty uses the public API.")
	flag.StringVar(&apiTokenKey, "api-token-key", "token",
		"Key within the default BorgBase API token Secret.")
	flag.StringVar(&backupImage, "backup-image", defaultBackupImage,
		"Image used for backup and init jobs. It must provide restic, runitor, ts and dumpdb.")
	flag.StringVar(&cacheStorageClass, "cache-storage-class", "",
		"StorageClass for restic cache volumes. Must support ReadWriteMany when backups overlap.")
	flag.BoolVar(&healthchecksEnabled, "healthchecks-enabled", true,
		"Report backup runs to healthchecks via runitor. Each ScheduledBackup still supplies its "+
			"own project ping key; there is deliberately no cluster-wide key.")
	flag.StringVar(&healthchecksAPIURL, "healthchecks-api-url", "http://healthchecks.healthchecks:8000/ping",
		"Healthchecks ping endpoint.")
	flag.BoolVar(&healthchecksAutoCreate, "healthchecks-auto-create", true,
		"Auto-provision a check on its first ping. Auto-created checks get the healthchecks "+
			"defaults of a one day period and one hour grace.")

	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	webhookTLSOpts := tlsOpts
	webhookServerOptions := webhook.Options{
		TLSOpts: webhookTLSOpts,
	}

	if len(webhookCertPath) > 0 {
		setupLog.Info("Initializing webhook certificate watcher using provided certificates",
			"webhook-cert-path", webhookCertPath, "webhook-cert-name", webhookCertName, "webhook-cert-key", webhookCertKey)

		webhookServerOptions.CertDir = webhookCertPath
		webhookServerOptions.CertName = webhookCertName
		webhookServerOptions.KeyName = webhookCertKey
	}

	webhookServer := webhook.NewServer(webhookServerOptions)

	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	managedByOperator := cache.ByObject{
		Label: labels.SelectorFromSet(labels.Set{
			"app.kubernetes.io/managed-by": "borgbase-operator",
		}),
	}

	tokenSecret, err := parseNamespacedName(apiTokenSecret, os.Getenv("POD_NAMESPACE"))
	if err != nil {
		setupLog.Error(err, "Invalid --api-token-secret")
		os.Exit(1)
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:  scheme,
		Metrics: metricsServerOptions,
		Cache: cache.Options{
			ByObject: map[client.Object]cache.ByObject{
				&batchv1.Job{}:     managedByOperator,
				&batchv1.CronJob{}: managedByOperator,
			},
		},
		Client: client.Options{
			Cache: &client.CacheOptions{
				// A cached client builds an informer over every object of the type, not
				// just the handful read by name. Caching Secrets holds every Helm
				// release and pull secret in the cluster in memory, which is what was
				// OOM killing the manager.
				DisableFor: []client.Object{
					&corev1.Secret{},
					&corev1.PersistentVolumeClaim{},
				},
			},
		},
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "936ba482.clevyr.com",
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	if err := (&controller.RepositoryReconciler{
		Client:             mgr.GetClient(),
		Scheme:             mgr.GetScheme(),
		DefaultTokenSecret: tokenSecret,
		DefaultTokenKey:    apiTokenKey,
		BackupImage:        backupImage,
		Endpoint:           borgbaseEndpoint,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "repository")
		os.Exit(1)
	}
	if err := (&controller.ScheduledBackupReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Config: backup.Config{
			Image:             backupImage,
			CacheStorageClass: cacheStorageClass,
			Healthchecks: healthchecks.Config{
				Enabled:    healthchecksEnabled,
				APIURL:     healthchecksAPIURL,
				AutoCreate: healthchecksAutoCreate,
			},
		},
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "Failed to create controller", "controller", "scheduledbackup")
		os.Exit(1)
	}
	// +kubebuilder:scaffold:builder

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "Failed to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("Starting manager")
	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		setupLog.Error(err, "Failed to run manager")
		os.Exit(1)
	}
}

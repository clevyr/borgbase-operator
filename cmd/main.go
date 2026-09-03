package main

import (
	"crypto/tls"
	"flag"
	"fmt"
	"os"
	"strings"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	"sigs.k8s.io/controller-runtime/pkg/metrics/filters"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	"sigs.k8s.io/controller-runtime/pkg/webhook"

	"k8s.io/apimachinery/pkg/types"

	borgbasev1 "github.com/clevyr/borgbase-operator/api/v1"
	"github.com/clevyr/borgbase-operator/internal/backup"
	"github.com/clevyr/borgbase-operator/internal/controller"
	"github.com/clevyr/borgbase-operator/internal/healthchecks"
	// +kubebuilder:scaffold:imports
)

// defaultBackupImage is the Clevyr restic image, which bundles restic,
// runitor, ts and the dumpdb helper the generated scripts rely on.
const defaultBackupImage = "ghcr.io/clevyr/restic:0.18.1"

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")
)

// parseNamespacedName parses a "namespace/name" value, falling back to
// defaultNS for a bare name.
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

// nolint:gocyclo
func main() {
	var metricsAddr string
	var metricsCertPath, metricsCertName, metricsCertKey string
	var webhookCertPath, webhookCertName, webhookCertKey string
	var enableLeaderElection bool
	var probeAddr string
	var secureMetrics bool
	var enableHTTP2 bool
	var tlsOpts []func(*tls.Config)

	// Operator-level configuration.
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
		Development: true,
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

	// if the enable-http2 flag is false (the default), http/2 should be disabled
	// due to its vulnerabilities. More specifically, disabling http/2 will
	// prevent from being vulnerable to the HTTP/2 Stream Cancellation and
	// Rapid Reset CVEs. For more information see:
	// - https://github.com/advisories/GHSA-qppj-fm5r-hxr3
	// - https://github.com/advisories/GHSA-4374-p667-p6c8
	disableHTTP2 := func(c *tls.Config) {
		setupLog.Info("Disabling HTTP/2")
		c.NextProtos = []string{"http/1.1"}
	}

	if !enableHTTP2 {
		tlsOpts = append(tlsOpts, disableHTTP2)
	}

	// Initial webhook TLS options
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

	// Metrics endpoint is enabled in 'config/default/kustomization.yaml'. The Metrics options configure the server.
	// More info:
	// - https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/server
	// - https://book.kubebuilder.io/reference/metrics.html
	metricsServerOptions := metricsserver.Options{
		BindAddress:   metricsAddr,
		SecureServing: secureMetrics,
		TLSOpts:       tlsOpts,
	}

	if secureMetrics {
		// FilterProvider is used to protect the metrics endpoint with authn/authz.
		// These configurations ensure that only authorized users and service accounts
		// can access the metrics endpoint. The RBAC are configured in 'config/rbac/kustomization.yaml'. More info:
		// https://pkg.go.dev/sigs.k8s.io/controller-runtime@v0.24.1/pkg/metrics/filters#WithAuthenticationAndAuthorization
		metricsServerOptions.FilterProvider = filters.WithAuthenticationAndAuthorization
	}

	// Without an explicit certificate, controller-runtime self-signs one for the
	// metrics server. That is fine here because metrics are not exposed outside
	// the cluster; serving them publicly would want a real certificate, via the
	// METRICS-WITH-CERTS and PROMETHEUS-WITH-CERTS sections in config/.
	if len(metricsCertPath) > 0 {
		setupLog.Info("Initializing metrics certificate watcher using provided certificates",
			"metrics-cert-path", metricsCertPath, "metrics-cert-name", metricsCertName, "metrics-cert-key", metricsCertKey)

		metricsServerOptions.CertDir = metricsCertPath
		metricsServerOptions.CertName = metricsCertName
		metricsServerOptions.KeyName = metricsCertKey
	}

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsServerOptions,
		WebhookServer:          webhookServer,
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "936ba482.clevyr.com",
		// LeaderElectionReleaseOnCancel defines if the leader should step down voluntarily
		// when the Manager ends. This requires the binary to immediately end when the
		// Manager is stopped, otherwise, this setting is unsafe. Setting this significantly
		// speeds up voluntary leader transitions as the new leader don't have to wait
		// LeaseDuration time first.
		//
		// In the default scaffold provided, the program ends immediately after
		// the manager stops, so would be fine to enable this option. However,
		// if you are doing or is intended to do any operation such as perform cleanups
		// after the manager stops then its usage might be unsafe.
		// LeaderElectionReleaseOnCancel: true,
	})
	if err != nil {
		setupLog.Error(err, "Failed to start manager")
		os.Exit(1)
	}

	tokenSecret, err := parseNamespacedName(apiTokenSecret, os.Getenv("POD_NAMESPACE"))
	if err != nil {
		setupLog.Error(err, "Invalid --api-token-secret")
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

package main

import (
	"context"
	"crypto/rand"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"runtime/debug"
	"strings"
	"time"

	// Import all Kubernetes client auth plugins (e.g. Azure, GCP, OIDC, etc.)
	// to ensure that exec-entrypoint and run can make use of them.
	_ "k8s.io/client-go/plugin/pkg/client/auth"

	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
	webhookserver "sigs.k8s.io/controller-runtime/pkg/webhook"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/internal/async"
	"github.com/divergedev/diverge/internal/changeset"
	"github.com/divergedev/diverge/internal/controller"
	"github.com/divergedev/diverge/internal/database"
	_ "github.com/divergedev/diverge/internal/database/cnpg"
	"github.com/divergedev/diverge/internal/deployer"
	"github.com/divergedev/diverge/internal/events"
	_ "github.com/divergedev/diverge/internal/metrics"
	"github.com/divergedev/diverge/internal/notifier"
	"github.com/divergedev/diverge/internal/observability"
	"github.com/divergedev/diverge/internal/routing"
	_ "github.com/divergedev/diverge/internal/sandbox"
	divtesting "github.com/divergedev/diverge/internal/testing"
	"github.com/divergedev/diverge/internal/webhook"
	pkgauth "github.com/divergedev/diverge/pkg/auth"
	pkgdb "github.com/divergedev/diverge/pkg/database"
	pkgfeatures "github.com/divergedev/diverge/pkg/features"
	"github.com/divergedev/diverge/pkg/registry"
	pkgsandbox "github.com/divergedev/diverge/pkg/sandbox"
)

var (
	scheme   = runtime.NewScheme()
	setupLog = ctrl.Log.WithName("setup")

	// Set via ldflags: -X main.version=... -X main.commit=... -X main.date=...
	version = "dev"
	commit  = "none"
	date    = "unknown"
)

func init() {
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))

	utilruntime.Must(divergeiov1alpha1.AddToScheme(scheme))
	// +kubebuilder:scaffold:scheme
}

func main() {
	var metricsAddr string
	var enableLeaderElection bool
	var probeAddr string
	var webhookPort int
	var routingProvider string
	var deployProvider string
	var databaseProvider string
	var webhookSecretToken string
	var gitlabAllowedUsersRaw string
	var notifierProvider string
	var defaultNamespace string
	var asyncProvider string
	var featureProvider string
	var enableAgentSandbox bool
	var sandboxProvider string
	var enableGAMMA bool

	flag.StringVar(&metricsAddr, "metrics-bind-address", ":8080", "The address the metric endpoint binds to.")
	flag.StringVar(&probeAddr, "health-probe-bind-address", ":8081", "The address the probe endpoint binds to.")
	flag.BoolVar(&enableLeaderElection, "leader-elect", false,
		"Enable leader election for controller manager. "+
			"Enabling this will ensure there is only one active controller manager.")
	flag.IntVar(&webhookPort, "webhook-port", 9443, "The port the webhook server binds to.")
	flag.StringVar(&routingProvider, "routing-provider", "gateway", "The routing provider to use (gateway, istio, composite, noop, or comma-separated list like 'gateway,istio').")
	flag.StringVar(&deployProvider, "deploy-provider", "noop", "Deployment provider (argocd|argo-rollouts|noop|direct|knative)")
	flag.StringVar(&databaseProvider, "database-provider", "none", "Database provider (schema|cnpg|snapshot|fresh|none)")
	flag.StringVar(&notifierProvider, "notifier-provider", "noop", "Notification provider (gitlab|github|noop)")
	flag.StringVar(&asyncProvider, "async-provider", "noop", "Async provisioning provider (noop, kafka, temporal, redis, valkey, webhook)")
	flag.StringVar(&featureProvider, "feature-provider", "configmap", "Feature flag provider (configmap, noop)")
	flag.BoolVar(&enableAgentSandbox, "enable-agent-sandbox", true, "Enable AgentTask controller and sandbox management")
	flag.StringVar(&sandboxProvider, "sandbox-provider", "agent-sandbox", "Default sandbox provider (agent-sandbox, noop)")
	flag.BoolVar(&enableGAMMA, "enable-gamma", false, "Enable GAMMA mesh routing for PreviewGroups (requires Istio Ambient or GAMMA-compatible mesh)")
	flag.StringVar(&webhookSecretToken, "webhook-secret-token", "", "The secret token for authenticating webhooks (prefer DIVERGE_WEBHOOK_SECRET env var).")
	flag.StringVar(&gitlabAllowedUsersRaw, "gitlab-allowed-users", "", "Comma-separated list of numeric GitLab user IDs authorized to execute /diverge MR slash commands (or set DIVERGE_GITLAB_ALLOWED_USERS).")
	flag.StringVar(&defaultNamespace, "default-namespace", "default", "Default namespace to create environments in")

	var kedaMinReplicas int64
	var kedaMaxReplicas int64
	var kedaCooldown int64
	flag.Int64Var(&kedaMinReplicas, "keda-min-replicas", 0, "Minimum replicas for KEDA scaling")
	flag.Int64Var(&kedaMaxReplicas, "keda-max-replicas", 3, "Maximum replicas for KEDA scaling")
	flag.Int64Var(&kedaCooldown, "keda-cooldown", 300, "Cooldown period in seconds for KEDA scaling")

	opts := zap.Options{
		Development: true,
	}
	opts.BindFlags(flag.CommandLine)
	flag.Parse()

	if kedaMinReplicas < 0 {
		setupLog.Error(fmt.Errorf("--keda-min-replicas must be >= 0, got %d", kedaMinReplicas), "invalid flag")
		os.Exit(1)
	}
	if kedaMaxReplicas == 0 {
		kedaMaxReplicas = 3 // default
	}
	if kedaMaxReplicas < kedaMinReplicas {
		setupLog.Error(fmt.Errorf("--keda-max-replicas (%d) must be >= --keda-min-replicas (%d)", kedaMaxReplicas, kedaMinReplicas), "invalid flag")
		os.Exit(1)
	}
	if kedaCooldown < 0 {
		setupLog.Error(fmt.Errorf("--keda-cooldown must be >= 0, got %d", kedaCooldown), "invalid flag")
		os.Exit(1)
	}

	// Version fallback for go install users
	if version == "dev" {
		if bi, ok := debug.ReadBuildInfo(); ok && bi.Main.Version != "" {
			version = bi.Main.Version
		}
	}
	setupLog.Info("starting diverge-controller", "version", version, "commit", commit, "date", date)

	// C2: Read secrets from environment variables (take precedence over flags)
	notifierToken := os.Getenv("DIVERGE_NOTIFIER_TOKEN")
	if envSecret := os.Getenv("DIVERGE_WEBHOOK_SECRET"); envSecret != "" {
		webhookSecretToken = envSecret
	}
	if envAllowed, ok := os.LookupEnv("DIVERGE_GITLAB_ALLOWED_USERS"); ok {
		gitlabAllowedUsersRaw = envAllowed
	}
	var gitlabAllowedUsers []string
	for _, u := range strings.Split(gitlabAllowedUsersRaw, ",") {
		if trimmed := strings.TrimSpace(u); trimmed != "" {
			gitlabAllowedUsers = append(gitlabAllowedUsers, trimmed)
		}
	}

	ctrl.SetLogger(zap.New(zap.UseFlagOptions(&opts)))

	ctx := ctrl.SetupSignalHandler()

	shutdownTracing, err := observability.Setup(ctx, "diverge-controller")
	if err != nil {
		setupLog.Error(err, "failed to setup tracing")
		os.Exit(1)
	}
	defer func() {
		if err := shutdownTracing(context.Background()); err != nil {
			setupLog.Error(err, "tracing shutdown error")
		}
	}()

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: metricsAddr},
		HealthProbeBindAddress: probeAddr,
		LeaderElection:         enableLeaderElection,
		LeaderElectionID:       "divergedev.com",
		WebhookServer:          webhookserver.NewServer(webhookserver.Options{Port: webhookPort}),
	})
	if err != nil {
		setupLog.Error(err, "unable to start manager")
		os.Exit(1)
	}

	deps := registry.Deps{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
		Logger: ctrl.Log,
	}

	var routerImpl routing.Router
	routerImpl, err = routing.NewRouter(routingProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating router", "provider", routingProvider)
		os.Exit(1)
	}

	// Wrap with metrics
	routerImpl = &routing.InstrumentedRouter{Inner: routerImpl, Mode: routingProvider}

	if databaseProvider == "" {
		databaseProvider = "none"
	}
	dbProviderImpl, err := pkgdb.Providers.Create(databaseProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating database provider", "provider", databaseProvider)
		os.Exit(1)
	}
	if closer, ok := dbProviderImpl.(io.Closer); ok {
		defer func() {
			if closeErr := closer.Close(); closeErr != nil {
				setupLog.Error(closeErr, "closing database provider")
			}
		}()
	}

	// Wrap with metrics
	dbProviderImpl = &database.InstrumentedDatabaseProvider{Inner: dbProviderImpl, Mode: databaseProvider}
	detectorImpl := &changeset.GitChangeDetector{}

	if notifierProvider == "" {
		notifierProvider = "noop"
	}
	notifierImpl, err := notifier.Providers.Create(notifierProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating notifier", "provider", notifierProvider)
		os.Exit(1)
	}

	statusReporterImpl, err := notifier.StatusProviders.Create(notifierProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating status reporter", "provider", notifierProvider)
		os.Exit(1)
	}

	if deployProvider == "" {
		deployProvider = "noop"
	}
	deployerImpl, err := deployer.Providers.Create(deployProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating deployer", "provider", deployProvider)
		os.Exit(1)
	}

	// Wrap with KEDA Deployer (detects CRD automatically)
	if deployProvider != "noop" {
		deployerImpl = &deployer.KEDADeployer{
			Inner:  deployerImpl,
			Client: mgr.GetClient(),
			Config: deployer.KEDAConfig{
				MinReplicas: kedaMinReplicas,
				MaxReplicas: kedaMaxReplicas,
				Cooldown:    kedaCooldown,
			},
		}
	}

	// Normalize label for metrics
	if deployProvider == "" {
		deployProvider = "noop"
	}

	// Wrap with metrics
	deployerImpl = &deployer.InstrumentedDeployer{Inner: deployerImpl, Name: deployProvider}

	testRunnerImpl, err := divtesting.Providers.Create(notifierProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating test runner", "provider", notifierProvider)
		os.Exit(1)
	}

	if asyncProvider == "" {
		asyncProvider = "noop"
	}
	asyncProviderImpl, err := async.Providers.Create(asyncProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating async provisioner", "provider", asyncProvider)
		os.Exit(1)
	}

	if featureProvider == "" {
		featureProvider = "configmap"
	}
	featureProviderImpl, err := pkgfeatures.Providers.Create(featureProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating feature provider", "provider", featureProvider)
		os.Exit(1)
	}

	if err = (&controller.EnvironmentReconciler{
		Client:           mgr.GetClient(),
		APIReader:        mgr.GetAPIReader(),
		Scheme:           mgr.GetScheme(),
		Recorder:         events.NewRecorder(mgr.GetEventRecorder("diverge-controller")),
		Router:           routerImpl,
		DatabaseProvider: dbProviderImpl,
		ChangeDetector:   detectorImpl,
		Notifier:         notifierImpl,
		StatusReporter:   statusReporterImpl,
		Deployer:         deployerImpl,
		TestRunner:       testRunnerImpl,
		AsyncProvisioner: asyncProviderImpl,
		FeatureProvider:  featureProviderImpl,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "Environment")
		os.Exit(1)
	}

	pgNotifierImpl, err := notifier.GroupProviders.Create(notifierProvider, deps)
	if err != nil {
		setupLog.Error(err, "creating preview group notifier", "provider", notifierProvider)
		os.Exit(1)
	}

	if err = (&controller.PreviewGroupReconciler{
		Client:           mgr.GetClient(),
		Scheme:           mgr.GetScheme(),
		Recorder:         events.NewRecorder(mgr.GetEventRecorder("diverge-previewgroup")),
		Notifier:         pgNotifierImpl,
		StatusReporter:   statusReporterImpl,
		DatabaseProvider: dbProviderImpl,
		EnableGAMMA:      enableGAMMA,
	}).SetupWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create controller", "controller", "PreviewGroup")
		os.Exit(1)
	}

	if enableAgentSandbox {
		var macaroonKey []byte
		if rawKey := os.Getenv("DIVERGE_MACAROON_ROOT_KEY"); rawKey != "" {
			if len(rawKey) < pkgauth.MinMacaroonRootKeyLen {
				setupLog.Error(fmt.Errorf("DIVERGE_MACAROON_ROOT_KEY must be at least %d bytes (got %d)", pkgauth.MinMacaroonRootKeyLen, len(rawKey)), "invalid macaroon root key")
				os.Exit(1)
			}
			macaroonKey = []byte(rawKey)
		} else {
			macaroonKey = make([]byte, pkgauth.MinMacaroonRootKeyLen)
			if _, err := rand.Read(macaroonKey); err != nil {
				setupLog.Error(err, "failed to generate macaroon root key")
				os.Exit(1)
			}
			setupLog.Info("generated ephemeral macaroon root key; set DIVERGE_MACAROON_ROOT_KEY for persistent token verification")
		}
		taskNotifier := resolveAgentTaskNotifier(notifierProvider, notifierToken, mgr.GetClient())
		if err = (&controller.AgentTaskReconciler{
			Client:          mgr.GetClient(),
			Scheme:          mgr.GetScheme(),
			SandboxRegistry: pkgsandbox.Providers,
			TokenMinter:     pkgauth.NewMacaroonProvider(macaroonKey),
			Notifier:        taskNotifier,
			FeatureGate:     &controller.StaticFeatureGate{Enabled: true},
			DefaultProvider: sandboxProvider,
		}).SetupWithManager(mgr); err != nil {
			setupLog.Error(err, "unable to create controller", "controller", "AgentTask")
			os.Exit(1)
		}
	} else {
		setupLog.Info("AgentTask controller is disabled via --enable-agent-sandbox=false")
	}
	// +kubebuilder:scaffold:builder

	if err = divergeiov1alpha1.SetupEnvironmentWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "Environment")
		os.Exit(1)
	}
	if err = divergeiov1alpha1.SetupPreviewGroupWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "PreviewGroup")
		os.Exit(1)
	}
	if err = divergeiov1alpha1.SetupAgentTaskWebhookWithManager(mgr); err != nil {
		setupLog.Error(err, "unable to create webhook", "webhook", "AgentTask")
		os.Exit(1)
	}

	// Setup webhook server for GitLab events
	webhookConfig := webhook.WebhookConfig{SecretToken: webhookSecretToken}

	var glConfigFetcher webhook.ConfigFetcher
	var glCommenter webhook.CommentPoster
	if notifierProvider == "gitlab" && notifierToken != "" {
		glConfigFetcher = &webhook.GitLabConfigFetcher{
			Token:      notifierToken,
			HTTPClient: &http.Client{Timeout: 15 * time.Second},
		}
		glCommenter = notifier.NewGitLabNotifier(firstNonEmpty(os.Getenv("DIVERGE_BASE_URL"), os.Getenv("GITLAB_API_URL")), notifierToken)
	}
	if notifierProvider == "gitlab" && len(gitlabAllowedUsers) == 0 {
		setupLog.Info("WARNING: GitLab MR slash commands are disabled because no allowed user IDs are configured; set --gitlab-allowed-users or DIVERGE_GITLAB_ALLOWED_USERS")
	}
	mgr.GetWebhookServer().Register("/gitlab-webhook", &webhook.GitLabWebhookHandler{
		Client:        mgr.GetClient(),
		Config:        webhookConfig,
		ConfigFetcher: glConfigFetcher,
		Commenter:     glCommenter,
		AllowedUsers:  gitlabAllowedUsers,
		DefaultNS:     defaultNamespace,
	})

	if notifierProvider == "gitlab" && notifierToken != "" {
		mgr.GetWebhookServer().Register("/gitlab-previewgroup-webhook", &webhook.GitLabPreviewGroupWebhookHandler{
			Client:        mgr.GetClient(),
			Config:        webhookConfig,
			ConfigFetcher: glConfigFetcher,
		})
	}

	var ghConfigFetcher webhook.ConfigFetcher
	var ghCommenter webhook.CommentPoster
	if notifierProvider == "github" && notifierToken != "" {
		ghConfigFetcher = &webhook.GitHubConfigFetcher{
			Token:      notifierToken,
			HTTPClient: &http.Client{Timeout: 15 * time.Second},
		}
		ghCommenter = notifier.NewGitHubNotifier(firstNonEmpty(os.Getenv("DIVERGE_BASE_URL"), os.Getenv("GITHUB_API_URL")), notifierToken)
	}
	mgr.GetWebhookServer().Register("/github-webhook", &webhook.GitHubWebhookHandler{
		Client:        mgr.GetClient(),
		Config:        webhookConfig,
		ConfigFetcher: ghConfigFetcher,
		Commenter:     ghCommenter,
		DefaultNS:     defaultNamespace,
	})

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up health check")
		os.Exit(1)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		setupLog.Error(err, "unable to set up ready check")
		os.Exit(1)
	}

	setupLog.Info("starting manager")
	if err := mgr.Start(ctx); err != nil {
		setupLog.Error(err, "problem running manager")
		os.Exit(1)
	}
}

func resolveAgentTaskNotifier(provider, explicitToken string, k8sClient client.Client) controller.TaskNotifier {
	var token, baseURL string
	switch strings.ToLower(strings.TrimSpace(provider)) {
	case "github":
		token = firstNonEmpty(explicitToken, os.Getenv("DIVERGE_NOTIFIER_TOKEN"), os.Getenv("GITHUB_TOKEN"))
		baseURL = firstNonEmpty(os.Getenv("DIVERGE_BASE_URL"), os.Getenv("GITHUB_API_URL"))
	case "gitlab":
		token = firstNonEmpty(explicitToken, os.Getenv("DIVERGE_NOTIFIER_TOKEN"), os.Getenv("DIVERGE_GITLAB_TOKEN"), os.Getenv("GITLAB_TOKEN"))
		baseURL = firstNonEmpty(os.Getenv("DIVERGE_BASE_URL"), os.Getenv("GITLAB_API_URL"))
	default:
		token = explicitToken
		baseURL = os.Getenv("DIVERGE_BASE_URL")
	}
	if tn := notifier.NewTaskNotifier(provider, token, baseURL, k8sClient); tn != nil {
		return tn
	}
	return &controller.NoopTaskNotifier{}
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if trimmed := strings.TrimSpace(v); trimmed != "" {
			return trimmed
		}
	}
	return ""
}

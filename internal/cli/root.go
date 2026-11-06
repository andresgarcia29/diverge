// Package cli implements the diverge command-line interface, providing commands
// for creating, listing, deleting, and managing preview environments.
package cli

import (
	"context"
	"fmt"
	"os"
	"os/signal"

	"github.com/spf13/cobra"
	"k8s.io/client-go/kubernetes"

	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
)

// App holds shared CLI state including Kubernetes connection details, version
// metadata, and optional API clients supplied by tests.
type App struct {
	Kubeconfig string
	Namespace  string
	Context    string
	NoColor    bool
	Version    string
	Commit     string
	Date       string

	Client    client.Client        // For testing
	Clientset kubernetes.Interface // For testing (logs needs CoreV1)
}

var lazyRootCmd *cobra.Command
var lazyApp *App

// RootCmd returns the root cobra command. External modules (e.g.,
// diverge-enterprise) use this to add enterprise subcommands.
func RootCmd() *cobra.Command {
	if lazyRootCmd == nil {
		lazyApp = &App{}
		lazyRootCmd = NewRootCmd(lazyApp)
	}
	return lazyRootCmd
}

// Execute is the main entry point for the diverge CLI. It injects version metadata
// into the App, initializes the root command if needed, and runs it.
func Execute(version, commit, date string) {
	if lazyApp == nil {
		lazyApp = &App{}
		lazyRootCmd = NewRootCmd(lazyApp)
	}
	lazyApp.Version = version
	lazyApp.Commit = commit
	lazyApp.Date = date

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, os.Interrupt)
	defer cancel()

	err := lazyRootCmd.ExecuteContext(ctx)
	if err != nil {
		os.Exit(1)
	}
}

// NewRootCmd constructs the root cobra.Command with persistent flags for kubeconfig,
// namespace, and context, and registers all subcommands (create, delete, init, list,
// logs, open, status, validate, version).
func NewRootCmd(app *App) *cobra.Command {
	rootCmd := &cobra.Command{
		Use:   "diverge",
		Short: "Diverge CLI manages preview environments",
		Long:  `The developer's daily driver for interacting with Diverge environments.`,
		PersistentPreRunE: func(cmd *cobra.Command, args []string) error {
			if !cmdRequiresKube(cmd) {
				return nil
			}
			return app.ResolveNamespace()
		},
	}

	defaultNamespace := app.Namespace
	rootCmd.PersistentFlags().StringVar(&app.Kubeconfig, "kubeconfig", "", "path to kubeconfig (default: ~/.kube/config)")
	rootCmd.PersistentFlags().StringVarP(&app.Namespace, "namespace", "n", defaultNamespace, "Kubernetes namespace (default: from kubeconfig context)")
	rootCmd.PersistentFlags().StringVar(&app.Context, "context", "", "Kubernetes context")
	rootCmd.PersistentFlags().BoolVar(&app.NoColor, "no-color", false, "disable color output")

	addCommands(rootCmd, app)
	return rootCmd
}

func cmdRequiresKube(cmd *cobra.Command) bool {
	for c := cmd; c != nil; c = c.Parent() {
		switch c.Name() {
		case "loadtest", "test", "version", "diff", "completion", "init", "validate", "curl", "verify", "replay", "setup", "mcp":
			return false
		}
	}
	return true
}

func addCommands(root *cobra.Command, app *App) {
	root.AddCommand(newCompletionCmd(app))
	root.AddCommand(newCreateCmd(app))
	root.AddCommand(newCurlCmd(app))
	root.AddCommand(newDeleteCmd(app))
	root.AddCommand(newDevCmd(app))
	root.AddCommand(newEnvCmd(app))
	root.AddCommand(newInitCmd(app))
	root.AddCommand(newListCmd(app))
	root.AddCommand(newLogsCmd(app))
	root.AddCommand(newOpenCmd(app))
	root.AddCommand(newPluginsCmd(app))
	root.AddCommand(newPreviewCmd(app))
	root.AddCommand(newProvidersCmd(app))
	root.AddCommand(newRouteCmd(app))
	root.AddCommand(newStatusCmd(app))
	root.AddCommand(newTopCmd(app))
	root.AddCommand(newValidateCmd(app))
	root.AddCommand(newVerifyCmd(app))
	root.AddCommand(newReplayCmd(app))
	root.AddCommand(newVersionCmd(app))
	root.AddCommand(newMCPCmd(app))
	root.AddCommand(newGraphCmd(app))
	root.AddCommand(newDiffCmd(app))
	root.AddCommand(newLoadtestCmd(app))
	root.AddCommand(newTestCmd(app))
	root.AddCommand(newDoctorCmd(app))
	root.AddCommand(newTaskCmd(app))
}

// ResolveNamespace resolves the namespace from the kubeconfig if not
// explicitly set via --namespace flag.
func (app *App) ResolveNamespace() error {
	if app.Namespace != "" {
		return nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if app.Kubeconfig != "" {
		loadingRules.ExplicitPath = app.Kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: app.Context,
	}
	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	ns, _, err := kubeConfig.Namespace()
	if err != nil {
		return fmt.Errorf("failed to resolve namespace from kubeconfig: %w", err)
	}
	app.Namespace = ns
	return nil
}

// KubeClient returns a controller-runtime client and a kubernetes.Clientset.
// It returns injected clients when app.Client is non-nil; otherwise it creates
// both clients from kubeconfig.
func (app *App) KubeClient() (client.Client, kubernetes.Interface, error) {
	if app.Client != nil {
		return app.Client, app.Clientset, nil
	}

	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if app.Kubeconfig != "" {
		loadingRules.ExplicitPath = app.Kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: app.Context,
	}
	if app.Namespace != "" {
		configOverrides.Context.Namespace = app.Namespace
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)

	config, err := kubeConfig.ClientConfig()
	if err != nil {
		return nil, nil, err
	}

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(divergeiov1alpha1.AddToScheme(scheme))

	c, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		return nil, nil, err
	}

	clientset, err := kubernetes.NewForConfig(config)
	if err != nil {
		return nil, nil, err
	}

	return c, clientset, nil
}

// RestConfig returns the REST config.
func (app *App) RestConfig() (*rest.Config, error) {
	loadingRules := clientcmd.NewDefaultClientConfigLoadingRules()
	if app.Kubeconfig != "" {
		loadingRules.ExplicitPath = app.Kubeconfig
	}
	configOverrides := &clientcmd.ConfigOverrides{
		CurrentContext: app.Context,
	}
	if app.Namespace != "" {
		configOverrides.Context.Namespace = app.Namespace
	}

	kubeConfig := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(loadingRules, configOverrides)
	return kubeConfig.ClientConfig()
}

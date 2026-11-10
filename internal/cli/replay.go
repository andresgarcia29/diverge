package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	urlPkg "net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/replay"
)

type replayOptions struct {
	traceFile          string
	baseURL            string
	previewURL         string
	headerKey          string
	headerValue        string
	concurrency        int
	timeout            time.Duration
	maxLatency         time.Duration
	compareValues      bool
	failOnExtraFields  bool
	failOnStatusDiff   bool
	failOnContractDiff bool
	ignoreFields       []string
	filterHosts        []string
	stripHeaders       []string
	stripAuth          bool
	redactQueryParams  []string
	redactBodyFields   []string
	protoDescriptor    string
	protoMessage       string
	outputJSON         bool
	githubSummary      bool
	autoFix            bool
}

// newReplayCmd constructs the `diverge replay` subcommand for deterministic traffic replay and contract diffing.
func newReplayCmd(app *App) *cobra.Command {
	opts := &replayOptions{}

	cmd := &cobra.Command{
		Use:   "replay <env-name> --from <trace.jsonl|trace.har|trace.json>",
		Short: "Replay recorded HTTP, ConnectRPC, and Protobuf/gRPC traffic against baseline and preview",
		Long: `Replays a deterministic request trace (.jsonl, .har, or .json) against both baseline
(unrouted) and a candidate preview Environment or PreviewGroup (with x-diverge-env and W3C
baggage injected).

Compares HTTP status code parity, per-request and p50/p95 latency, JSON structural schemas,
and binary Protobuf / gRPC / ConnectRPC wire tags (or typed descriptors via --proto-descriptor).`,
		Example: `	# Replay a JSONL trace against preview environment pr-42
	diverge replay pr-42 --from ./traces/checkout.jsonl

	# Replay a browser/Playwright HAR file and write a GitHub Step Summary
	diverge replay pr-42 --from ./recording.har --base-url https://staging.example.com --github-summary

	# Replay binary Protobuf/gRPC traffic with a compiled FileDescriptorSet (.binpb)
	diverge replay pr-42 --from ./grpc-trace.jsonl --proto-descriptor ./descriptors.binpb --proto-message acme.v1.ChargeResponse`,
		Args:              cobra.ExactArgs(1),
		SilenceUsage:      true,
		ValidArgsFunction: completeEnvironmentNames(app),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]
			if strings.TrimSpace(opts.traceFile) == "" {
				return fmt.Errorf("--from trace file path is required")
			}

			entries, err := replay.LoadTrace(opts.traceFile)
			if err != nil {
				return err
			}
			if len(opts.filterHosts) > 0 {
				entries = replay.FilterEntriesByHost(entries, opts.filterHosts)
				if len(entries) == 0 {
					return fmt.Errorf("no trace entries matched --filter-host %v", opts.filterHosts)
				}
			}

			protoFiles, err := replay.LoadDescriptorFiles(opts.protoDescriptor)
			if err != nil {
				return err
			}

			baseURL, previewURL, headerKey, headerValue, ns, err := resolveReplayRouting(cmd, app, envName, entries, opts)
			if err != nil {
				return err
			}

			cfg := replay.Config{
				Environment:        envName,
				Namespace:          ns,
				BaseURL:            baseURL,
				PreviewBaseURL:     previewURL,
				HeaderKey:          headerKey,
				HeaderValue:        headerValue,
				Concurrency:        opts.concurrency,
				Timeout:            opts.timeout,
				MaxLatency:         opts.maxLatency,
				FailOnStatusDiff:   opts.failOnStatusDiff,
				FailOnContractDiff: opts.failOnContractDiff,
				FilterHosts:        opts.filterHosts,
				StripHeaders:       opts.stripHeaders,
				StripAuth:          opts.stripAuth,
				RedactQueryParams:  opts.redactQueryParams,
				RedactBodyFields:   opts.redactBodyFields,
				DiffOptions: replay.DiffOptions{
					CompareValues:     opts.compareValues,
					FailOnExtraFields: opts.failOnExtraFields,
					IgnoreFields:      opts.ignoreFields,
					ProtoFiles:        protoFiles,
					ProtoMessage:      opts.protoMessage,
				},
			}

			runner := replay.NewRunner(cfg)
			report, err := runner.Run(cmd.Context(), entries)
			if err != nil {
				return err
			}

			out := cmd.OutOrStdout()
			if opts.outputJSON {
				if err := replay.FormatJSON(out, report); err != nil {
					return fmt.Errorf("failed to encode replay report: %w", err)
				}
			} else {
				replay.FormatTable(out, report)
				if os.Getenv("GITHUB_ACTIONS") == "true" {
					for _, r := range report.Results {
						if !r.Passed {
							title := escapeGHAProperty(fmt.Sprintf("Diverge Replay (%s %s)", r.Method, r.Path))
							msg := strings.Join(r.FailureReasons, "; ")
							_, _ = fmt.Fprintf(out, "::error title=%s::%s\n", title, escapeGHAData(msg))
						}
					}
				}
			}

			if opts.githubSummary {
				if summaryPath := strings.TrimSpace(os.Getenv("GITHUB_STEP_SUMMARY")); summaryPath != "" {
					f, err := os.OpenFile(summaryPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
					if err != nil {
						return fmt.Errorf("failed to write GITHUB_STEP_SUMMARY: %w", err)
					}
					writeErr := replay.FormatMarkdown(f, report)
					closeErr := f.Close()
					if writeErr != nil {
						return fmt.Errorf("failed to write GITHUB_STEP_SUMMARY: %w", writeErr)
					}
					if closeErr != nil {
						return fmt.Errorf("failed to close GITHUB_STEP_SUMMARY: %w", closeErr)
					}
				} else {
					summaryOut := out
					if opts.outputJSON {
						summaryOut = cmd.ErrOrStderr()
					} else {
						_, _ = fmt.Fprintln(summaryOut)
					}
					if err := replay.FormatMarkdown(summaryOut, report); err != nil {
						return fmt.Errorf("failed to write Markdown summary: %w", err)
					}
				}
			}

			if !report.Passed {
				if opts.autoFix {
					var mdBuf bytes.Buffer
					_ = replay.FormatMarkdown(&mdBuf, report)
					statusWriter := out
					if opts.outputJSON {
						statusWriter = cmd.ErrOrStderr()
					}
					if taskName, fixErr := triggerCLIAutoFixTask(cmd.Context(), app, envName, ns, "replay", mdBuf.String()); fixErr != nil {
						_, _ = fmt.Fprintf(cmd.ErrOrStderr(), "Warning: failed to trigger self-healing AgentTask: %v\n", fixErr)
					} else {
						_, _ = fmt.Fprintf(statusWriter, "Triggered self-healing AgentTask %s in namespace %s\n", taskName, ns)
					}
				}
				return fmt.Errorf("traffic replay failed for environment %s (%d/%d requests failed)", envName, report.FailedRequests, report.TotalRequests)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&opts.traceFile, "from", "f", "", "Path to request trace file (.jsonl, .har, .json, or '-' for stdin)")
	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "Override baseline gateway URL")
	cmd.Flags().StringVar(&opts.previewURL, "preview-url", "", "Optional separate preview base URL (for subdomain-routed environments)")
	cmd.Flags().StringVar(&opts.headerKey, "header-key", "", "Override routing header key (default: x-diverge-env)")
	cmd.Flags().StringVar(&opts.headerValue, "header-value", "", "Override routing header value (default: environment name)")
	cmd.Flags().IntVarP(&opts.concurrency, "concurrency", "c", replay.DefaultConcurrency, "Number of concurrent replay workers")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", replay.DefaultTimeout, "Per-request HTTP timeout")
	cmd.Flags().DurationVar(&opts.maxLatency, "max-latency", 0, "Maximum acceptable preview response latency per request (e.g. 500ms)")
	cmd.Flags().BoolVar(&opts.compareValues, "compare-values", false, "Compare exact scalar values in addition to JSON/Protobuf field types and wire schema")
	cmd.Flags().BoolVar(&opts.failOnExtraFields, "fail-on-extra-fields", false, "Treat newly added JSON keys or Protobuf tags in preview as breaking")
	cmd.Flags().BoolVar(&opts.failOnStatusDiff, "fail-on-status-diff", true, "Fail if baseline and preview HTTP status codes differ")
	cmd.Flags().BoolVar(&opts.failOnContractDiff, "fail-on-contract-diff", true, "Fail if breaking JSON or Protobuf contract diffs are detected")
	cmd.Flags().StringSliceVar(&opts.ignoreFields, "ignore-field", nil, "Field name or path to ignore during diffing (repeatable, e.g. --ignore-field request_id)")
	cmd.Flags().StringSliceVar(&opts.filterHosts, "filter-host", nil, "Retain only trace entries matching the given hostname or wildcard pattern (repeatable, e.g. --filter-host api.example.com --filter-host '*.example.com')")
	cmd.Flags().StringSliceVar(&opts.stripHeaders, "strip-header", nil, "Request header name to strip before replaying (repeatable, e.g. --strip-header X-Internal-Token)")
	cmd.Flags().BoolVar(&opts.stripAuth, "strip-auth", false, "Strip Authorization, Proxy-Authorization, Cookie, X-Api-Key headers and auth query params before replaying")
	cmd.Flags().StringSliceVar(&opts.redactQueryParams, "redact-query", nil, "Query parameter name to redact with [REDACTED] before replaying (repeatable)")
	cmd.Flags().StringSliceVar(&opts.redactBodyFields, "redact-body-field", nil, "JSON request body field name or dot-path to redact before replaying (repeatable)")
	cmd.Flags().StringVar(&opts.protoDescriptor, "proto-descriptor", "", "Optional path to compiled Protobuf FileDescriptorSet (.binpb) for schema-aware decoding")
	cmd.Flags().StringVar(&opts.protoMessage, "proto-message", "", "Default fully-qualified Protobuf message type for response decoding")
	cmd.Flags().BoolVar(&opts.outputJSON, "json", false, "Output replay report as JSON")
	cmd.Flags().BoolVar(&opts.githubSummary, "github-summary", false, "Write Markdown replay report to $GITHUB_STEP_SUMMARY (or stdout, or stderr when --json is set)")
	cmd.Flags().BoolVar(&opts.autoFix, "auto-fix", false, "Automatically create or update an autonomous AgentTask when contract replay fails")

	cmd.AddCommand(newReplaySanitizeCmd())

	return cmd
}

type replaySanitizeOptions struct {
	fromFile          string
	outputFile        string
	stripAuth         bool
	stripHeaders      []string
	redactQueryParams []string
	redactBodyFields  []string
	filterHosts       []string
}

// newReplaySanitizeCmd constructs the `diverge replay sanitize` subcommand to convert and scrub raw HAR/JSON/JSONL traces into clean JSONL files.
func newReplaySanitizeCmd() *cobra.Command {
	opts := &replaySanitizeOptions{}

	cmd := &cobra.Command{
		Use:   "sanitize --from <raw.har|raw.json|raw.jsonl> [--output <clean.jsonl>]",
		Short: "Convert and scrub credentials/PII from a HAR, JSON, or JSONL trace into a clean JSONL file",
		Long: `Loads a raw browser/Playwright HAR (.har), JSON (.json), or JSONL (.jsonl) trace,
filters entries by host if requested, strips authentication headers/cookies/query tokens,
redacts sensitive JSON request body fields, and writes a deterministic, git-committable
JSONL trace.`,
		Example: `	# Sanitize a browser HAR export into a clean JSONL trace for Git
	diverge replay sanitize --from ./recording.har --output ./traces/checkout.jsonl --filter-host api.example.com --redact-body-field password,cvv`,
		Args:         cobra.NoArgs,
		SilenceUsage: true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			if strings.TrimSpace(opts.fromFile) == "" {
				return fmt.Errorf("--from trace file path is required")
			}

			entries, err := replay.LoadTrace(opts.fromFile)
			if err != nil {
				return err
			}

			sanitized := replay.SanitizeEntries(entries, replay.SanitizeOptions{
				StripAuth:         opts.stripAuth,
				StripHeaders:      opts.stripHeaders,
				RedactQueryParams: opts.redactQueryParams,
				RedactBodyFields:  opts.redactBodyFields,
				FilterHosts:       opts.filterHosts,
			})
			if len(sanitized) == 0 {
				return fmt.Errorf("0 trace entries remaining after sanitization/filtering")
			}

			outWriter := cmd.OutOrStdout()
			outPath := strings.TrimSpace(opts.outputFile)
			if outPath != "" && outPath != "-" {
				f, err := os.Create(outPath)
				if err != nil {
					return fmt.Errorf("create output trace file %s: %w", outPath, err)
				}
				defer func() { _ = f.Close() }()
				outWriter = f
			}

			if err := replay.WriteJSONL(outWriter, sanitized); err != nil {
				return err
			}

			if outPath != "" && outPath != "-" {
				_, _ = fmt.Fprintf(cmd.OutOrStdout(), "Sanitized %d trace entries to %s\n", len(sanitized), outPath)
			}
			return nil
		},
	}

	cmd.Flags().StringVarP(&opts.fromFile, "from", "f", "", "Input trace file path (.har, .json, .jsonl, or '-' for stdin)")
	cmd.Flags().StringVarP(&opts.outputFile, "output", "o", "", "Output JSONL file path (default: stdout)")
	cmd.Flags().BoolVar(&opts.stripAuth, "strip-auth", true, "Strip Authorization, Cookie, X-Api-Key headers, userinfo, and auth query tokens")
	cmd.Flags().StringSliceVar(&opts.stripHeaders, "strip-header", nil, "Additional request header name to strip (repeatable)")
	cmd.Flags().StringSliceVar(&opts.redactQueryParams, "redact-query", nil, "Additional query parameter name to redact with [REDACTED] (repeatable)")
	cmd.Flags().StringSliceVar(&opts.redactBodyFields, "redact-body-field", nil, "JSON request body field name or dot-path to redact with [REDACTED] (repeatable)")
	cmd.Flags().StringSliceVar(&opts.filterHosts, "filter-host", nil, "Retain only entries matching the given hostname or wildcard pattern (repeatable)")

	return cmd
}

// triggerCLIAutoFixTask creates or updates an autonomous AgentTask in Kubernetes seeded with the failure summary.
func triggerCLIAutoFixTask(ctx context.Context, app *App, envName, ns, failureKind, markdownSummary string) (string, error) {
	if app == nil {
		return "", fmt.Errorf("cluster application context is nil")
	}
	c, _, err := app.KubeClient()
	if err != nil || c == nil {
		return "", fmt.Errorf("connect to cluster: %w", err)
	}
	if strings.TrimSpace(ns) == "" {
		ns = "default"
	}

	repoURL := ""
	branch := envName

	var env divergeiov1alpha1.Environment
	if getErr := c.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &env); getErr == nil {
		repoURL, branch = resolveSourceRepoAndBranch(env.Spec.Source, envName)
	} else {
		var pg divergeiov1alpha1.PreviewGroup
		if pgErr := c.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &pg); pgErr == nil {
			repoURL, branch = resolveSourceRepoAndBranch(pg.Spec.Source, envName)
		} else if pgErr2 := c.Get(ctx, client.ObjectKey{Name: envName}, &pg); pgErr2 == nil {
			repoURL, branch = resolveSourceRepoAndBranch(pg.Spec.Source, envName)
		}
	}
	if repoURL == "" {
		return "", fmt.Errorf("cannot resolve source repository for %s; set spec.source.project", envName)
	}

	taskName := sanitizeDNSLabel(fmt.Sprintf("autofix-%s-%s", failureKind, envName))
	if len(taskName) > 50 {
		taskName = strings.TrimRight(taskName[:50], "-")
	}
	objective := fmt.Sprintf("Fix %s regression detected on preview environment %s:\n\n%s", failureKind, envName, strings.TrimSpace(markdownSummary))

	var existing divergeiov1alpha1.AgentTask
	getErr := c.Get(ctx, client.ObjectKey{Name: taskName, Namespace: ns}, &existing)
	if getErr == nil {
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		existing.Annotations["agent.diverge.io/guidance"] = objective
		existing.Spec.Objective = objective
		existing.Spec.Suspended = false
		if err := c.Update(ctx, &existing); err != nil {
			return "", fmt.Errorf("update existing AgentTask %s: %w", taskName, err)
		}
		return taskName, nil
	}
	if !apierrors.IsNotFound(getErr) {
		return "", fmt.Errorf("get AgentTask %s: %w", taskName, getErr)
	}

	task := &divergeiov1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: ns,
			Labels: map[string]string{
				"diverge.io/environment": envName,
				"diverge.io/autofix":     failureKind,
			},
		},
		Spec: divergeiov1alpha1.AgentTaskSpec{
			Objective: objective,
			Repository: divergeiov1alpha1.AgentTaskRepository{
				URL:           repoURL,
				BaseBranch:    "main",
				WorkingBranch: branch,
			},
			MaxIterations: 5,
			DraftPR:       true,
		},
	}
	if err := c.Create(ctx, task); err != nil {
		return "", fmt.Errorf("create AgentTask %s: %w", taskName, err)
	}
	return taskName, nil
}

// resolveSourceRepoAndBranch derives a clone URL and working branch from an EnvironmentSource.
func resolveSourceRepoAndBranch(src divergeiov1alpha1.EnvironmentSource, fallbackEnv string) (string, string) {
	branch := strings.TrimSpace(src.Branch)
	if branch == "" {
		branch = fallbackEnv
	}
	project := strings.TrimSpace(src.Project)
	if project == "" {
		return "", branch
	}
	if strings.HasPrefix(project, "http://") || strings.HasPrefix(project, "https://") || strings.HasPrefix(project, "git@") {
		return project, branch
	}
	if strings.EqualFold(src.Provider, "gitlab") {
		return "https://gitlab.com/" + project, branch
	}
	return "https://github.com/" + project, branch
}

// resolveReplayRouting resolves the baseline URL, preview URL, routing header key/value, and namespace for a replay run.
func resolveReplayRouting(cmd *cobra.Command, app *App, envName string, entries []replay.RequestEntry, opts *replayOptions) (string, string, string, string, string, error) {
	baseURL := strings.TrimSpace(opts.baseURL)
	previewURL := strings.TrimSpace(opts.previewURL)
	headerKey := replay.DefaultHeaderKey
	headerValue := envName

	explicitHeaderRouting := strings.TrimSpace(opts.headerKey) != "" || strings.TrimSpace(opts.headerValue) != ""
	if cmd != nil && (cmd.Flags().Changed("header-key") || cmd.Flags().Changed("header-value")) {
		explicitHeaderRouting = true
	}
	allAbsolute := allEntriesAbsolute(entries)
	canSkipCluster := (previewURL != "" || explicitHeaderRouting) && (baseURL != "" || allAbsolute)

	ns := "default"
	if app != nil {
		if app.Namespace == "" {
			if err := app.ResolveNamespace(); err != nil {
				if !canSkipCluster {
					return "", "", "", "", "", err
				}
			}
		}
		if app.Namespace != "" {
			ns = app.Namespace
		}
	}

	subdomainRouted := false
	var (
		clusterPreviewURL       string
		clusterBaselineURL      string
		ambiguousPreviewService bool
	)

	if app != nil {
		c, _, kErr := app.KubeClient()
		if kErr == nil && c != nil {
			var env divergeiov1alpha1.Environment
			if getErr := c.Get(cmd.Context(), client.ObjectKey{Name: envName, Namespace: ns}, &env); getErr == nil {
				subdomainRouted = strings.EqualFold(strings.TrimSpace(env.Spec.Routing.Mode), "subdomain")
				if env.Spec.Routing.HeaderKey != "" {
					headerKey = env.Spec.Routing.HeaderKey
				}
				if env.Spec.Routing.HeaderValue != "" {
					headerValue = env.Spec.Routing.HeaderValue
				}
				if subdomainRouted {
					clusterPreviewURL = strings.TrimSpace(env.Status.URL)
				} else {
					clusterBaselineURL = strings.TrimSpace(env.Status.URL)
				}
			} else {
				var pg divergeiov1alpha1.PreviewGroup
				pgErr := c.Get(cmd.Context(), client.ObjectKey{Name: envName, Namespace: ns}, &pg)
				if pgErr != nil {
					pgErr = c.Get(cmd.Context(), client.ObjectKey{Name: envName}, &pg)
				}
				if pgErr == nil {
					subdomainRouted = strings.EqualFold(strings.TrimSpace(pg.Spec.Routing.Mode), "subdomain")
					if pg.Spec.Routing.HeaderKey != "" {
						headerKey = pg.Spec.Routing.HeaderKey
					}
					if pg.Spec.Routing.HeaderValue != "" {
						headerValue = pg.Spec.Routing.HeaderValue
					}
					extURL := strings.TrimSpace(pg.Spec.Routing.ExternalURL)
					if !subdomainRouted && !strings.Contains(extURL, "{env}") {
						clusterBaselineURL = extURL
					}
					clusterPreviewURL, ambiguousPreviewService = selectPreviewGroupServiceURL(pg.Status.Services, baseURL, extURL, entries)
				} else if !canSkipCluster {
					return "", "", "", "", ns, fmt.Errorf("failed to get environment %s (pass --preview-url or --header-key/--header-value to replay without cluster discovery): %w", envName, getErr)
				}
			}
		} else if !canSkipCluster {
			return "", "", "", "", ns, fmt.Errorf("failed to connect to cluster (pass --preview-url or --header-key/--header-value to replay without cluster discovery): %w", kErr)
		}
	} else if !canSkipCluster {
		return "", "", "", "", ns, fmt.Errorf("cannot resolve routing for %s without cluster connection; pass --preview-url or --header-key/--header-value", envName)
	}

	if strings.TrimSpace(opts.headerKey) != "" {
		headerKey = strings.TrimSpace(opts.headerKey)
	}
	if strings.TrimSpace(opts.headerValue) != "" {
		headerValue = strings.TrimSpace(opts.headerValue)
	}

	if ambiguousPreviewService && previewURL == "" && (subdomainRouted || (baseURL == "" && clusterBaselineURL == "" && !allAbsolute)) {
		return "", "", "", "", ns, fmt.Errorf("preview group %s has multiple service URLs; pass --preview-url to select the target preview service", envName)
	}

	if subdomainRouted {
		if previewURL == "" {
			previewURL = clusterPreviewURL
		}
		if previewURL == "" {
			return "", "", "", "", ns, fmt.Errorf(
				"environment %s uses subdomain routing but has no published preview URL; pass --preview-url or wait for the preview URL to be published",
				envName,
			)
		}
		if baseURL == "" {
			baseURL = clusterBaselineURL
		}
		if baseURL == "" && !allAbsolute {
			return "", "", "", "", ns, fmt.Errorf(
				"environment %s uses subdomain routing (%s); pass --base-url <baseline-url> so baseline and preview requests target their respective hosts",
				envName, previewURL,
			)
		}
		return baseURL, previewURL, headerKey, headerValue, ns, nil
	}

	if baseURL == "" {
		baseURL = clusterBaselineURL
		if baseURL == "" && !allAbsolute {
			baseURL = clusterPreviewURL
		}
	}
	if baseURL == "" && !allAbsolute {
		return "", "", "", "", ns, fmt.Errorf("environment %s has no baseline URL; pass --base-url <url>", envName)
	}
	if previewURL == "" {
		previewURL = baseURL
	}

	return baseURL, previewURL, headerKey, headerValue, ns, nil
}

// allEntriesAbsolute reports whether every request entry in entries specifies an absolute http:// or https:// URL.
func allEntriesAbsolute(entries []replay.RequestEntry) bool {
	if len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		if !isAbsoluteHTTPURL(e.Path) {
			return false
		}
	}
	return true
}

// selectPreviewGroupServiceURL selects an unambiguous service URL from a PreviewGroup's status services.
func selectPreviewGroupServiceURL(services []divergeiov1alpha1.PreviewGroupServiceStatus, explicitBaseURL, externalURL string, entries []replay.RequestEntry) (string, bool) {
	var candidates []divergeiov1alpha1.PreviewGroupServiceStatus
	for _, svc := range services {
		if strings.TrimSpace(svc.URL) != "" {
			candidates = append(candidates, svc)
		}
	}
	if len(candidates) == 0 {
		return "", false
	}
	if len(candidates) == 1 {
		return strings.TrimSpace(candidates[0].URL), false
	}

	refURL := strings.TrimSpace(explicitBaseURL)
	if refURL == "" && len(entries) > 0 && isAbsoluteHTTPURL(entries[0].Path) {
		refURL = strings.TrimSpace(entries[0].Path)
	}
	if refURL == "" && !strings.Contains(externalURL, "{") {
		refURL = strings.TrimSpace(externalURL)
	}
	if refURL == "" {
		return "", true
	}

	parsed, err := urlPkg.Parse(refURL)
	if err != nil || parsed.Hostname() == "" {
		return "", true
	}
	host := strings.ToLower(parsed.Hostname())
	firstLabel, _, _ := strings.Cut(host, ".")

	// 1. Check exact published service URL hostname or explicit {env} URL-template match.
	var templateMatched []string
	for _, svc := range candidates {
		svcURL := strings.TrimSpace(svc.URL)
		if svcParsed, err := urlPkg.Parse(svcURL); err == nil && strings.EqualFold(svcParsed.Hostname(), host) {
			templateMatched = append(templateMatched, svcURL)
			continue
		}
		if strings.Contains(externalURL, "{env}") {
			for _, token := range []string{strings.TrimSpace(svc.EnvironmentName), strings.TrimSpace(svc.Name)} {
				if token == "" {
					continue
				}
				expanded := strings.ReplaceAll(externalURL, "{env}", token)
				if expParsed, err := urlPkg.Parse(expanded); err == nil && strings.EqualFold(expParsed.Hostname(), host) {
					templateMatched = append(templateMatched, svcURL)
					break
				}
			}
		}
	}
	if len(templateMatched) == 1 {
		return templateMatched[0], false
	}
	if len(templateMatched) > 1 {
		return "", true
	}

	// 2. Check generated child Environment name match (svc.EnvironmentName or pg-{group}-{service}-{hash8}).
	var childEnvMatched []string
	for _, svc := range candidates {
		if matchesPreviewGroupChildEnv(host, firstLabel, svc) {
			childEnvMatched = append(childEnvMatched, strings.TrimSpace(svc.URL))
		}
	}
	if len(childEnvMatched) == 1 {
		return childEnvMatched[0], false
	}
	if len(childEnvMatched) > 1 {
		return "", true
	}

	// 3. Match hyphen-delimited service tokens within the first DNS label or host.
	// Overlapping names (such as "web" and "web-admin" against "pr-42-web-admin.example.com")
	// match multiple candidates and return an ambiguity result.
	var matched []string
	for _, svc := range candidates {
		name := sanitizeDNSLabel(svc.Name)
		if name != "" && (host == name || labelMatchesServiceToken(firstLabel, name)) {
			matched = append(matched, strings.TrimSpace(svc.URL))
		}
	}
	if len(matched) == 1 {
		return matched[0], false
	}
	return "", true
}

// matchesPreviewGroupChildEnv reports whether host or firstLabel matches a PreviewGroup child Environment name.
func matchesPreviewGroupChildEnv(host, firstLabel string, svc divergeiov1alpha1.PreviewGroupServiceStatus) bool {
	if envName := sanitizeDNSLabel(svc.EnvironmentName); envName != "" {
		if host == envName || labelMatchesServiceToken(firstLabel, envName) {
			return true
		}
	}
	if svcParsed, err := urlPkg.Parse(strings.TrimSpace(svc.URL)); err == nil {
		svcFirstLabel, _, _ := strings.Cut(strings.ToLower(svcParsed.Hostname()), ".")
		if isGeneratedChildEnvLabel(svcFirstLabel, svc.Name) && (host == svcFirstLabel || labelMatchesServiceToken(firstLabel, svcFirstLabel)) {
			return true
		}
	}
	return isGeneratedChildEnvLabel(firstLabel, svc.Name)
}

// isGeneratedChildEnvLabel reports whether label contains a generated child Environment name (pg-{group}-{service}-{hash8}) for serviceName.
func isGeneratedChildEnvLabel(label, serviceName string) bool {
	sanitizedSvc := sanitizeDNSLabel(serviceName)
	if sanitizedSvc == "" {
		return false
	}
	for offset := 0; offset < len(label); {
		rel := strings.Index(label[offset:], "pg-")
		if rel < 0 {
			break
		}
		idx := offset + rel
		offset = idx + 3
		if idx > 0 && label[idx-1] != '-' {
			continue
		}
		rem := label[idx:]
		if len(rem) <= 12 {
			continue
		}
		// Look for -<sanitizedSvc>-<8 hex chars> at the end of rem or before a hyphen delimiter
		needle := "-" + sanitizedSvc + "-"
		for searchFrom := 3; searchFrom < len(rem); {
			pos := strings.Index(rem[searchFrom:], needle)
			if pos < 0 {
				break
			}
			matchIdx := searchFrom + pos
			searchFrom = matchIdx + 1
			group := rem[3:matchIdx]
			if group == "" {
				continue
			}
			hashStart := matchIdx + len(needle)
			if hashStart+8 > len(rem) {
				continue
			}
			if hashStart+8 < len(rem) && rem[hashStart+8] != '-' {
				continue
			}
			hashPart := rem[hashStart : hashStart+8]
			if !isLowerHex8(hashPart) {
				continue
			}
			sum := sha256.Sum256([]byte(group + "/" + strings.TrimSpace(serviceName)))
			if fmt.Sprintf("%x", sum)[:8] == hashPart {
				return true
			}
		}
	}
	return false
}

// isLowerHex8 reports whether s is an 8-character lowercase hexadecimal string.
func isLowerHex8(s string) bool {
	if len(s) != 8 {
		return false
	}
	for i := 0; i < 8; i++ {
		c := s[i]
		isDigit := c >= '0' && c <= '9'
		isHexAlpha := c >= 'a' && c <= 'f'
		if !isDigit && !isHexAlpha {
			return false
		}
	}
	return true
}

// sanitizeDNSLabel normalizes a service or environment identifier into a lowercase hyphenated DNS token.
func sanitizeDNSLabel(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	prevDash := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9') {
			b.WriteByte(c)
			prevDash = false
		} else if c == '-' || c == '.' || c == '_' {
			if !prevDash && b.Len() > 0 {
				b.WriteByte('-')
				prevDash = true
			}
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// labelMatchesServiceToken reports whether label equals token or contains token bounded by hyphens.
func labelMatchesServiceToken(label, token string) bool {
	if label == "" || token == "" {
		return false
	}
	return label == token ||
		strings.HasPrefix(label, token+"-") ||
		strings.Contains(label, "-"+token+"-") ||
		strings.HasSuffix(label, "-"+token)
}

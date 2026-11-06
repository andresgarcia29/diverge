package cli

import (
	"fmt"
	urlPkg "net/url"
	"os"
	"strings"
	"time"

	"github.com/spf13/cobra"
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
	protoDescriptor    string
	protoMessage       string
	outputJSON         bool
	githubSummary      bool
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
	cmd.Flags().StringVar(&opts.protoDescriptor, "proto-descriptor", "", "Optional path to compiled Protobuf FileDescriptorSet (.binpb) for schema-aware decoding")
	cmd.Flags().StringVar(&opts.protoMessage, "proto-message", "", "Default fully-qualified Protobuf message type for response decoding")
	cmd.Flags().BoolVar(&opts.outputJSON, "json", false, "Output replay report as JSON")
	cmd.Flags().BoolVar(&opts.githubSummary, "github-summary", false, "Write Markdown replay report to $GITHUB_STEP_SUMMARY (or stdout, or stderr when --json is set)")

	return cmd
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
					clusterBaselineURL = strings.TrimSpace(pg.Spec.Routing.ExternalURL)
					clusterPreviewURL, ambiguousPreviewService = selectPreviewGroupServiceURL(pg.Status.Services, baseURL, clusterBaselineURL, entries)
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

	if ambiguousPreviewService && previewURL == "" {
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
		if baseURL == "" {
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

	refURL := explicitBaseURL
	if refURL == "" {
		refURL = externalURL
	}
	if refURL == "" && len(entries) > 0 && isAbsoluteHTTPURL(entries[0].Path) {
		refURL = entries[0].Path
	}
	if refURL != "" {
		if parsed, err := urlPkg.Parse(refURL); err == nil {
			host := strings.ToLower(parsed.Hostname())
			var matched []string
			for _, svc := range candidates {
				name := strings.ToLower(strings.TrimSpace(svc.Name))
				if name != "" && (host == name || strings.HasPrefix(host, name+".") || strings.Contains(host, "-"+name+"-") || strings.HasSuffix(host, "-"+name)) {
					matched = append(matched, strings.TrimSpace(svc.URL))
				}
			}
			if len(matched) == 1 {
				return matched[0], false
			}
		}
	}
	return "", true
}

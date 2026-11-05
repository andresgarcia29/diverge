package cli

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"text/tabwriter"
	"time"

	"github.com/spf13/cobra"
	"sigs.k8s.io/controller-runtime/pkg/client"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/doctor"
)

// VerifyCheckResult represents the outcome of an individual verification check.
type VerifyCheckResult struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Message    string `json:"message"`
}

// VerifyReport aggregates all verification checks and optional cluster diagnostics.
type VerifyReport struct {
	Environment   string              `json:"environment"`
	Namespace     string              `json:"namespace,omitempty"`
	TargetURL     string              `json:"targetUrl,omitempty"`
	RoutingHeader string              `json:"routingHeader,omitempty"`
	Passed        bool                `json:"passed"`
	Checks        []VerifyCheckResult `json:"checks"`
	Doctor        *doctor.Report      `json:"doctor,omitempty"`
}

type verifyOptions struct {
	baseURL          string
	paths            []string
	headerKey        string
	headerValue      string
	method           string
	data             string
	headers          []string
	expectStatus     int
	maxLatency       time.Duration
	bodyContains     string
	compareBaseline  bool
	failOnStatusDiff bool
	skipDoctor       bool
	timeout          time.Duration
	outputJSON       bool
	githubSummary    bool
}

func newVerifyCmd(app *App) *cobra.Command {
	opts := &verifyOptions{}

	cmd := &cobra.Command{
		Use:   "verify <env-name> [path-or-url]",
		Short: "Verify preview environment health, HTTP readiness, baseline parity, and latency SLOs",
		Long: `Runs a unified verification suite against a preview environment combining
Kubernetes workload diagnostics (doctor), routed HTTP readiness probes, latency SLO
assertions, response substring checks, and optional baseline parity comparisons.`,
		Example: `	# Verify environment health and default "/" HTTP endpoint
	diverge verify pr-42

	# Verify multiple endpoints with a latency SLO and baseline comparison
	diverge verify pr-42 --path /healthz --path /api/orders --max-latency 500ms --compare-baseline

	# Verify against an explicit URL in CI and emit JSON report
	diverge verify pr-42 /api/status --base-url https://preview.example.com --body-contains '"ok":true' --json`,
		Args:              cobra.RangeArgs(1, 2),
		SilenceUsage:      true,
		ValidArgsFunction: completeEnvironmentNames(app),
		RunE: func(cmd *cobra.Command, args []string) error {
			envName := args[0]

			pathsToProbe := opts.paths
			if len(args) > 1 {
				pathsToProbe = []string{args[1]}
			}
			if len(pathsToProbe) == 0 {
				pathsToProbe = []string{"/"}
			}
			firstPath := pathsToProbe[0]
			isFullURL := strings.HasPrefix(firstPath, "http://") || strings.HasPrefix(firstPath, "https://")

			if app != nil && app.Namespace == "" {
				_ = app.ResolveNamespace()
			}
			ns := "default"
			if app != nil && app.Namespace != "" {
				ns = app.Namespace
			}

			report := &VerifyReport{
				Environment: envName,
				Namespace:   ns,
				Checks:      make([]VerifyCheckResult, 0),
			}

			// 1. Doctor & Resource Health Check
			if !opts.skipDoctor && app != nil {
				c, _, err := app.KubeClient()
				if err == nil && c != nil {
					diagnoser := doctor.NewDiagnoser(c)
					docReport, docErr := diagnoser.Diagnose(cmd.Context(), ns, envName)
					if docErr != nil || docReport == nil {
						msg := "diagnostics returned no report"
						if docErr != nil {
							msg = docErr.Error()
						}
						report.Checks = append(report.Checks, VerifyCheckResult{
							Name: "workload-diagnostics", Passed: false, Status: "ERROR", Message: msg,
						})
					} else {
						report.Doctor = docReport
						docStatus := "HEALTHY"
						docMsg := "All preview pods, migrations, and routes healthy"
						if !docReport.Healthy {
							docStatus = "UNHEALTHY"
							docMsg = fmt.Sprintf("%d issue(s) detected", len(docReport.Issues))
						}
						report.Checks = append(report.Checks, VerifyCheckResult{
							Name:    "workload-diagnostics",
							Passed:  docReport.Healthy,
							Status:  docStatus,
							Message: docMsg,
						})
					}
				} else if err != nil && opts.baseURL == "" && !isFullURL {
					return fmt.Errorf("failed to connect to cluster: %w", err)
				} else if err != nil {
					report.Checks = append(report.Checks, VerifyCheckResult{
						Name:    "workload-diagnostics",
						Passed:  false,
						Status:  "ERROR",
						Message: fmt.Sprintf("cluster unavailable (use --skip-doctor to skip): %v", err),
					})
				}
			}

			// 2. Resolve Routing & Target Paths
			curlOpts := curlOptions{
				baseURL:     opts.baseURL,
				headerKey:   opts.headerKey,
				headerValue: opts.headerValue,
				method:      opts.method,
				data:        opts.data,
				headers:     opts.headers,
				timeout:     opts.timeout,
			}

			endpoints, headerKey, headerValue, err := resolveVerifyEndpoints(cmd, app, envName, firstPath, &curlOpts)
			if err != nil {
				return err
			}
			var targetURLs []string
			for _, ep := range endpoints {
				if ep.BaseURL != "" {
					targetURLs = append(targetURLs, ep.BaseURL)
				}
			}
			report.TargetURL = strings.Join(targetURLs, ", ")
			if headerKey != "" && headerValue != "" {
				report.RoutingHeader = fmt.Sprintf("%s: %s", headerKey, headerValue)
			}

			method := strings.ToUpper(strings.TrimSpace(opts.method))
			if method == "" {
				if opts.data != "" {
					method = http.MethodPost
				} else {
					method = http.MethodGet
				}
			}

			httpClient := &http.Client{
				Timeout: opts.timeout,
			}

			for _, ep := range endpoints {
				for _, p := range pathsToProbe {
					targetURL, err := buildCurlTargetURL(ep.BaseURL, p)
					if err != nil {
						return err
					}
					if report.TargetURL == "" {
						report.TargetURL = targetURL
					}

					prevResp, err := executeCurlRequest(cmd, httpClient, method, targetURL, opts.data, opts.headers, headerKey, headerValue)
					if err != nil {
						report.Checks = append(report.Checks, VerifyCheckResult{
							Name:    formatVerifyCheckName("http-status", ep.ServiceName, p),
							Passed:  false,
							Status:  "ERROR",
							Message: err.Error(),
						})
						continue
					}

					var statusPassed bool
					var statusMsg string
					if opts.expectStatus > 0 {
						statusPassed = prevResp.StatusCode == opts.expectStatus
						statusMsg = fmt.Sprintf("expected HTTP %d, got %d (%s)", opts.expectStatus, prevResp.StatusCode, targetURL)
					} else {
						statusPassed = prevResp.StatusCode >= 200 && prevResp.StatusCode < 400
						statusMsg = fmt.Sprintf("expected 2xx/3xx, got %d (%s)", prevResp.StatusCode, targetURL)
					}

					report.Checks = append(report.Checks, VerifyCheckResult{
						Name:       formatVerifyCheckName("http-status", ep.ServiceName, p),
						Passed:     statusPassed,
						Status:     fmt.Sprintf("HTTP %d", prevResp.StatusCode),
						DurationMs: prevResp.Duration.Milliseconds(),
						Message:    statusMsg,
					})

					if opts.maxLatency > 0 {
						report.Checks = append(report.Checks, VerifyCheckResult{
							Name:       formatVerifyCheckName("latency-slo", ep.ServiceName, p),
							Passed:     prevResp.Duration <= opts.maxLatency,
							Status:     prevResp.Duration.Round(time.Millisecond).String(),
							DurationMs: prevResp.Duration.Milliseconds(),
							Message:    fmt.Sprintf("threshold <= %s", opts.maxLatency),
						})
					}

					if opts.bodyContains != "" {
						matched := strings.Contains(prevResp.Body, opts.bodyContains)
						bodyStatus := "MATCH"
						bodyMsg := fmt.Sprintf("response body contains %q", opts.bodyContains)
						if !matched {
							bodyStatus = "MISMATCH"
							bodyMsg = fmt.Sprintf("response body missing %q", opts.bodyContains)
						}
						report.Checks = append(report.Checks, VerifyCheckResult{
							Name:    formatVerifyCheckName("body-contains", ep.ServiceName, p),
							Passed:  matched,
							Status:  bodyStatus,
							Message: bodyMsg,
						})
					}

					if opts.compareBaseline {
						baseResp, baseErr := executeCurlRequest(cmd, httpClient, method, targetURL, opts.data, opts.headers, "", "")
						if baseErr != nil {
							report.Checks = append(report.Checks, VerifyCheckResult{
								Name:    formatVerifyCheckName("baseline-parity", ep.ServiceName, p),
								Passed:  false,
								Status:  "ERROR",
								Message: fmt.Sprintf("baseline request failed: %v", baseErr),
							})
						} else {
							statusMatch := baseResp.StatusCode == prevResp.StatusCode
							passed := !opts.failOnStatusDiff || statusMatch
							parityStatus := "PARITY"
							if !statusMatch {
								parityStatus = "DIFF"
							}
							report.Checks = append(report.Checks, VerifyCheckResult{
								Name:       formatVerifyCheckName("baseline-parity", ep.ServiceName, p),
								Passed:     passed,
								Status:     parityStatus,
								DurationMs: baseResp.Duration.Milliseconds(),
								Message:    fmt.Sprintf("baseline HTTP %d (%s) vs preview HTTP %d (%s)", baseResp.StatusCode, baseResp.Duration.Round(time.Millisecond), prevResp.StatusCode, prevResp.Duration.Round(time.Millisecond)),
							})
						}
					}
				}
			}

			// 3. Aggregate & Format Output
			passedCount := 0
			failedCount := 0
			for _, chk := range report.Checks {
				if chk.Passed {
					passedCount++
				} else {
					failedCount++
				}
			}
			report.Passed = len(report.Checks) > 0 && failedCount == 0

			out := cmd.OutOrStdout()
			if opts.outputJSON {
				enc := json.NewEncoder(out)
				enc.SetIndent("", "  ")
				if err := enc.Encode(report); err != nil {
					return fmt.Errorf("failed to encode verification report: %w", err)
				}
			} else {
				renderVerifyTable(out, report, passedCount, failedCount)
				if os.Getenv("GITHUB_ACTIONS") == "true" {
					for _, c := range report.Checks {
						if !c.Passed {
							title := escapeGHAProperty(fmt.Sprintf("Diverge Verify (%s)", c.Name))
							_, _ = fmt.Fprintf(out, "::error title=%s::%s\n", title, escapeGHAData(c.Message))
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
					renderVerifyMarkdown(f, *report)
					_ = f.Close()
				} else if !opts.outputJSON {
					_, _ = fmt.Fprintln(out)
					renderVerifyMarkdown(out, *report)
				}
			}

			if !report.Passed {
				return fmt.Errorf("verification failed for environment %s", envName)
			}

			return nil
		},
	}

	cmd.Flags().StringVar(&opts.baseURL, "base-url", "", "Override the base URL for HTTP verification")
	cmd.Flags().StringSliceVar(&opts.paths, "path", []string{"/"}, "HTTP path(s) to probe")
	cmd.Flags().StringVar(&opts.headerKey, "header-key", "", "Override routing header key (default: x-diverge-env)")
	cmd.Flags().StringVar(&opts.headerValue, "header-value", "", "Override routing header value (default: environment name)")
	cmd.Flags().StringVarP(&opts.method, "method", "X", "GET", "HTTP request method")
	cmd.Flags().StringVarP(&opts.data, "data", "d", "", "HTTP request body")
	cmd.Flags().StringArrayVarP(&opts.headers, "header", "H", nil, "Additional HTTP header in 'Key: Value' format")
	cmd.Flags().IntVar(&opts.expectStatus, "expect-status", 0, "Expected HTTP status code (default 0 allows any 2xx/3xx status < 400)")
	cmd.Flags().DurationVar(&opts.maxLatency, "max-latency", 0, "Maximum acceptable preview response latency (e.g. 500ms)")
	cmd.Flags().StringVar(&opts.bodyContains, "body-contains", "", "Substring that must appear in the preview HTTP response body")
	cmd.Flags().BoolVar(&opts.compareBaseline, "compare-baseline", false, "Also probe baseline (without preview routing headers) and verify status parity")
	cmd.Flags().BoolVar(&opts.failOnStatusDiff, "fail-on-status-diff", true, "Fail verification if baseline and preview HTTP status codes differ when --compare-baseline is enabled")
	cmd.Flags().BoolVar(&opts.skipDoctor, "skip-doctor", false, "Skip Kubernetes workload health diagnostics (doctor)")
	cmd.Flags().DurationVar(&opts.timeout, "timeout", 15*time.Second, "Per-request HTTP timeout")
	cmd.Flags().BoolVar(&opts.outputJSON, "json", false, "Output verification report as JSON")
	cmd.Flags().BoolVar(&opts.githubSummary, "github-summary", false, "Write a Markdown verification scorecard to $GITHUB_STEP_SUMMARY (or stdout if unset)")

	return cmd
}

type verifyEndpoint struct {
	ServiceName string
	BaseURL     string
}

func formatVerifyCheckName(prefix, serviceName, path string) string {
	if serviceName != "" {
		return fmt.Sprintf("%s:%s:%s", prefix, serviceName, path)
	}
	return fmt.Sprintf("%s:%s", prefix, path)
}

func resolveVerifyEndpoints(cmd *cobra.Command, app *App, envName, firstPath string, opts *curlOptions) ([]verifyEndpoint, string, string, error) {
	baseURL, headerKey, headerValue, err := resolveCurlRouting(cmd, app, envName, firstPath, opts)
	if err != nil {
		return nil, "", "", err
	}

	isFullURL := strings.HasPrefix(firstPath, "http://") || strings.HasPrefix(firstPath, "https://")
	if strings.TrimSpace(opts.baseURL) == "" && !isFullURL && app != nil {
		if c, _, err := app.KubeClient(); err == nil && c != nil {
			ns := app.Namespace
			if ns == "" {
				ns = "default"
			}
			var env divergeiov1alpha1.Environment
			if getErr := c.Get(cmd.Context(), client.ObjectKey{Name: envName, Namespace: ns}, &env); getErr != nil {
				var pg divergeiov1alpha1.PreviewGroup
				pgErr := c.Get(cmd.Context(), client.ObjectKey{Name: envName, Namespace: ns}, &pg)
				if pgErr != nil {
					pgErr = c.Get(cmd.Context(), client.ObjectKey{Name: envName}, &pg)
				}
				if pgErr == nil && pg.Spec.Routing.ExternalURL == "" {
					var svcEndpoints []verifyEndpoint
					for _, svc := range pg.Status.Services {
						u := strings.TrimSpace(svc.URL)
						if u != "" {
							svcEndpoints = append(svcEndpoints, verifyEndpoint{
								ServiceName: svc.Name,
								BaseURL:     u,
							})
						}
					}
					if len(svcEndpoints) > 1 {
						return svcEndpoints, headerKey, headerValue, nil
					}
				}
			}
		}
	}

	return []verifyEndpoint{{ServiceName: "", BaseURL: baseURL}}, headerKey, headerValue, nil
}

func escapeGHAData(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A")
	return r.Replace(s)
}

func escapeGHAProperty(s string) string {
	r := strings.NewReplacer("%", "%25", "\r", "%0D", "\n", "%0A", ":", "%3A", ",", "%2C")
	return r.Replace(s)
}

func renderVerifyTable(out io.Writer, report *VerifyReport, passedCount, failedCount int) {
	tw := tabwriter.NewWriter(out, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "CHECK\tSTATUS\tRESULT\tDETAILS")
	for _, chk := range report.Checks {
		result := "[PASS]"
		if !chk.Passed {
			result = "[FAIL]"
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", chk.Name, chk.Status, result, chk.Message)
	}
	_ = tw.Flush()
	_, _ = fmt.Fprintln(out)
	if report.Passed {
		_, _ = fmt.Fprintf(out, "Verification PASSED (%d/%d checks)\n", passedCount, len(report.Checks))
	} else {
		_, _ = fmt.Fprintf(out, "Verification FAILED (%d/%d checks failed)\n", failedCount, len(report.Checks))
	}
}

func renderVerifyMarkdown(w io.Writer, report VerifyReport) {
	if report.Passed {
		_, _ = fmt.Fprintf(w, "### ✅ Diverge Verification Passed: %s\n\n", report.Environment)
	} else {
		_, _ = fmt.Fprintf(w, "### ❌ Diverge Verification Failed: %s\n\n", report.Environment)
	}

	urlStr := report.TargetURL
	if urlStr == "" {
		urlStr = "-"
	}
	headerStr := report.RoutingHeader
	if headerStr == "" {
		headerStr = fmt.Sprintf("x-diverge-env: %s", report.Environment)
	}
	nsStr := report.Namespace
	if nsStr == "" {
		nsStr = "default"
	}
	_, _ = fmt.Fprintf(w, "**URL:** %s | **Header:** `%s` | **Namespace:** `%s`\n\n", urlStr, headerStr, nsStr)

	_, _ = fmt.Fprintln(w, "| Check | Status | Latency | Result | Details |")
	_, _ = fmt.Fprintln(w, "| :--- | :--- | :--- | :--- | :--- |")
	for _, c := range report.Checks {
		latency := "-"
		if c.DurationMs > 0 {
			latency = fmt.Sprintf("%dms", c.DurationMs)
		} else if strings.HasPrefix(c.Name, "http-status:") || strings.HasPrefix(c.Name, "latency-slo:") || strings.HasPrefix(c.Name, "baseline-parity:") {
			latency = "0ms"
		}
		result := "✅ PASS"
		if !c.Passed {
			result = "❌ FAIL"
		}
		details := strings.ReplaceAll(c.Message, "|", "\\|")
		_, _ = fmt.Fprintf(w, "| %s | %s | %s | %s | %s |\n", c.Name, c.Status, latency, result, details)
	}

	if report.Doctor != nil && len(report.Doctor.Issues) > 0 {
		_, _ = fmt.Fprintln(w, "\n#### 🩺 Doctor Diagnostics")
		for _, issue := range report.Doctor.Issues {
			msg := issue.Summary
			if issue.Details != "" {
				msg = fmt.Sprintf("%s (%s)", issue.Summary, issue.Details)
			}
			_, _ = fmt.Fprintf(w, "- [%s] %s: %s\n", issue.Code, issue.Component, msg)
			if issue.Remediation != "" {
				_, _ = fmt.Fprintf(w, "  - 💡 Remediation: %s\n", issue.Remediation)
			}
		}
	}
}

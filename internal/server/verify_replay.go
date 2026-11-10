package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"regexp"
	"strings"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/doctor"
	"github.com/divergedev/diverge/pkg/loadtest"
	"github.com/divergedev/diverge/pkg/replay"
)

const maxVerifyReplayBodyBytes = 4 * 1024 * 1024 // 4 MiB request payload cap

// VerifyCheckItem represents a single verification check result returned to the web dashboard.
type VerifyCheckItem struct {
	Name       string `json:"name"`
	Passed     bool   `json:"passed"`
	Status     string `json:"status"`
	DurationMs int64  `json:"durationMs,omitempty"`
	Message    string `json:"message"`
}

// VerifyEnvironmentRequest is the JSON request body for POST /api/v1/environments/verify.
type VerifyEnvironmentRequest struct {
	Name            string   `json:"name"`
	Namespace       string   `json:"namespace,omitempty"`
	BaseURL         string   `json:"baseUrl,omitempty"`
	PreviewURL      string   `json:"previewUrl,omitempty"`
	Paths           []string `json:"paths,omitempty"`
	MaxLatencyMs    int64    `json:"maxLatencyMs,omitempty"`
	CompareBaseline bool     `json:"compareBaseline"`
	SkipDoctor      bool     `json:"skipDoctor,omitempty"`
	AutoFix         bool     `json:"autoFix,omitempty"`
}

// VerifyEnvironmentResponse is the JSON response body for POST /api/v1/environments/verify.
type VerifyEnvironmentResponse struct {
	Environment     string            `json:"environment"`
	Namespace       string            `json:"namespace"`
	TargetURL       string            `json:"targetUrl,omitempty"`
	PreviewURL      string            `json:"previewUrl,omitempty"`
	RoutingHeader   string            `json:"routingHeader,omitempty"`
	Passed          bool              `json:"passed"`
	Checks          []VerifyCheckItem `json:"checks"`
	Doctor          *doctor.Report    `json:"doctor,omitempty"`
	ReplayReport    *replay.Report    `json:"replayReport,omitempty"`
	AutoFixTaskName string            `json:"autoFixTaskName,omitempty"`
	AutoFixError    string            `json:"autoFixError,omitempty"`
}

// ReplayEnvironmentRequest is the JSON request body for POST /api/v1/environments/replay.
type ReplayEnvironmentRequest struct {
	Name              string                `json:"name"`
	Namespace         string                `json:"namespace,omitempty"`
	BaseURL           string                `json:"baseUrl,omitempty"`
	PreviewURL        string                `json:"previewUrl,omitempty"`
	TraceContent      string                `json:"traceContent,omitempty"`
	TraceFormat       string                `json:"traceFormat,omitempty"`
	Requests          []replay.RequestEntry `json:"requests,omitempty"`
	Sequential        bool                  `json:"sequential,omitempty"`
	CompareValues     bool                  `json:"compareValues,omitempty"`
	FailOnExtraFields bool                  `json:"failOnExtraFields,omitempty"`
	IgnoreFields      []string              `json:"ignoreFields,omitempty"`
	FilterHosts       []string              `json:"filterHosts,omitempty"`
	StripAuth         *bool                 `json:"stripAuth,omitempty"`
	StripHeaders      []string              `json:"stripHeaders,omitempty"`
	RedactQueryParams []string              `json:"redactQueryParams,omitempty"`
	RedactBodyFields  []string              `json:"redactBodyFields,omitempty"`
	MaxLatencyMs      int64                 `json:"maxLatencyMs,omitempty"`
	AutoFix           bool                  `json:"autoFix,omitempty"`
}

// ReplayEnvironmentResponse is the JSON response body for POST /api/v1/environments/replay.
type ReplayEnvironmentResponse struct {
	*replay.Report
	Markdown        string `json:"markdown,omitempty"`
	AutoFixTaskName string `json:"autoFixTaskName,omitempty"`
	AutoFixError    string `json:"autoFixError,omitempty"`
}

// VerifyReplayHandler serves HTTP endpoints for interactive environment verification and contract replay.
type VerifyReplayHandler struct {
	client      client.Client
	k8sClient   kubernetes.Interface
	logger      *slog.Logger
	auditLogger *AuditLogger
	httpClient  *http.Client
}

// NewVerifyReplayHandler constructs a new VerifyReplayHandler.
func NewVerifyReplayHandler(c client.Client, k8s kubernetes.Interface, logger *slog.Logger, audit *AuditLogger) *VerifyReplayHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &VerifyReplayHandler{
		client:      c,
		k8sClient:   k8s,
		logger:      logger,
		auditLogger: audit,
	}
}

// HandleVerify handles POST /api/v1/environments/verify.
func (h *VerifyReplayHandler) HandleVerify(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.client == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "cluster client not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxVerifyReplayBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > maxVerifyReplayBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4 MiB limit")
		return
	}

	var req VerifyEnvironmentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	envName := strings.TrimSpace(req.Name)
	if envName == "" {
		writeJSONError(w, http.StatusBadRequest, "environment name is required")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = "default"
	}
	if err := ValidateDNS1123Label(ns, "namespace"); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if h.k8sClient != nil {
		if err := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "get", ns, "environments"); err != nil {
			writeJSONError(w, http.StatusForbidden, "permission denied")
			return
		}
		if strings.TrimSpace(req.BaseURL) != "" || strings.TrimSpace(req.PreviewURL) != "" {
			if err := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "update", ns, "environments"); err != nil {
				writeJSONError(w, http.StatusForbidden, "permission denied for custom target URL override")
				return
			}
		}
	}

	var env divergeiov1alpha1.Environment
	if err := h.client.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &env); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("environment %s not found in namespace %s", envName, ns))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to look up environment")
		return
	}

	allowedHosts := collectAllowedEnvHosts(&env)
	baseURL, previewURL, headerKey, headerValue, subdomainRouted := resolveServerEnvRouting(&env, req.BaseURL, req.PreviewURL)
	if err := validateServerTargetHost(baseURL, allowedHosts); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateServerTargetHost(previewURL, allowedHosts); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	paths := req.Paths
	if len(paths) == 0 {
		paths = []string{"/"}
	}
	for _, p := range paths {
		trimmed := strings.ToLower(strings.TrimSpace(p))
		if strings.HasPrefix(trimmed, "http://") || strings.HasPrefix(trimmed, "https://") || strings.HasPrefix(trimmed, "//") {
			writeJSONError(w, http.StatusBadRequest, "verify paths must be relative paths")
			return
		}
	}

	resp := VerifyEnvironmentResponse{
		Environment: envName,
		Namespace:   ns,
		TargetURL:   baseURL,
		PreviewURL:  previewURL,
		Checks:      make([]VerifyCheckItem, 0),
	}
	if headerKey != "" && headerValue != "" {
		resp.RoutingHeader = fmt.Sprintf("%s: %s", headerKey, headerValue)
	}

	if !req.SkipDoctor {
		diagnoser := doctor.NewDiagnoser(h.client)
		docReport, docErr := diagnoser.Diagnose(ctx, ns, envName)
		if docErr != nil || docReport == nil {
			msg := "diagnostics returned no report"
			if docErr != nil {
				msg = docErr.Error()
			}
			resp.Checks = append(resp.Checks, VerifyCheckItem{
				Name:    "workload-diagnostics",
				Passed:  false,
				Status:  "ERROR",
				Message: msg,
			})
		} else {
			resp.Doctor = docReport
			status := "HEALTHY"
			msg := "All preview pods, migrations, and routes healthy"
			if !docReport.Healthy {
				status = "UNHEALTHY"
				msg = fmt.Sprintf("%d issue(s) detected", len(docReport.Issues))
			}
			resp.Checks = append(resp.Checks, VerifyCheckItem{
				Name:    "workload-diagnostics",
				Passed:  docReport.Healthy,
				Status:  status,
				Message: msg,
			})
		}
	}

	probeTarget := previewURL
	if probeTarget == "" {
		probeTarget = baseURL
	}
	if probeTarget != "" {
		entries := make([]replay.RequestEntry, 0, len(paths))
		for _, p := range paths {
			trimmed := strings.TrimSpace(p)
			if trimmed == "" {
				continue
			}
			if !strings.HasPrefix(trimmed, "/") {
				trimmed = "/" + trimmed
			}
			entries = append(entries, replay.RequestEntry{
				ID:     fmt.Sprintf("GET %s", trimmed),
				Method: "GET",
				Path:   trimmed,
			})
		}

		effectiveBase := baseURL
		if effectiveBase == "" || (!req.CompareBaseline && subdomainRouted) {
			effectiveBase = probeTarget
		}

		var maxLatency time.Duration
		if req.MaxLatencyMs > 0 {
			maxLatency = time.Duration(req.MaxLatencyMs) * time.Millisecond
		}

		repReport, repErr := replay.NewRunner(replay.Config{
			Environment:      envName,
			Namespace:        ns,
			BaseURL:          effectiveBase,
			PreviewBaseURL:   probeTarget,
			HeaderKey:        headerKey,
			HeaderValue:      headerValue,
			MaxLatency:       maxLatency,
			FailOnStatusDiff: req.CompareBaseline && baseURL != "",
			FilterHosts:      allowedHosts,
			StripAuth:        true,
			Timeout:          10 * time.Second,
			HTTPClient:       h.httpClient,
		}).Run(ctx, entries)
		if repErr != nil {
			resp.Checks = append(resp.Checks, VerifyCheckItem{
				Name:    "http-probe",
				Passed:  false,
				Status:  "ERROR",
				Message: repErr.Error(),
			})
		} else if repReport != nil {
			resp.ReplayReport = repReport
			for _, r := range repReport.Results {
				statusOk := r.PreviewStatus >= 200 && r.PreviewStatus < 400
				msg := fmt.Sprintf("preview HTTP %d (%dms)", r.PreviewStatus, r.PreviewLatencyMs)
				if req.CompareBaseline && baseURL != "" {
					msg = fmt.Sprintf("baseline HTTP %d (%dms) -> preview HTTP %d (%dms)", r.BaselineStatus, r.BaselineLatencyMs, r.PreviewStatus, r.PreviewLatencyMs)
				}
				if len(r.FailureReasons) > 0 {
					msg = strings.Join(r.FailureReasons, "; ")
				}
				resp.Checks = append(resp.Checks, VerifyCheckItem{
					Name:       fmt.Sprintf("http:%s", r.Path),
					Passed:     r.Passed && statusOk,
					Status:     fmt.Sprintf("HTTP %d", r.PreviewStatus),
					DurationMs: r.PreviewLatencyMs,
					Message:    msg,
				})
			}
		}
	}

	allPassed := len(resp.Checks) > 0
	for _, chk := range resp.Checks {
		if !chk.Passed {
			allPassed = false
			break
		}
	}
	resp.Passed = allPassed

	if !resp.Passed && req.AutoFix {
		var summary strings.Builder
		fmt.Fprintf(&summary, "### ❌ Diverge Verification Failed: %s\n\n", envName)
		for _, chk := range resp.Checks {
			if !chk.Passed {
				fmt.Fprintf(&summary, "- **%s** (%s): %s\n", chk.Name, chk.Status, chk.Message)
			}
		}
		if taskName, fixErr := h.createOrUpdateAutoFixTask(ctx, &env, "verify", summary.String()); fixErr != nil {
			h.logger.Warn("auto-fix task creation failed", "environment", envName, "namespace", ns, "error", fixErr)
			resp.AutoFixError = fixErr.Error()
		} else {
			resp.AutoFixTaskName = taskName
		}
	}

	writeJSONResponse(w, http.StatusOK, resp)
}

// HandleReplay handles POST /api/v1/environments/replay.
func (h *VerifyReplayHandler) HandleReplay(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.client == nil {
		writeJSONError(w, http.StatusServiceUnavailable, "cluster client not configured")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, maxVerifyReplayBodyBytes+1))
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, "failed to read request body")
		return
	}
	if len(body) > maxVerifyReplayBodyBytes {
		writeJSONError(w, http.StatusRequestEntityTooLarge, "request body exceeds 4 MiB limit")
		return
	}

	var req ReplayEnvironmentRequest
	if err := json.Unmarshal(body, &req); err != nil {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid JSON body: %v", err))
		return
	}

	envName := strings.TrimSpace(req.Name)
	if envName == "" {
		writeJSONError(w, http.StatusBadRequest, "environment name is required")
		return
	}
	ns := strings.TrimSpace(req.Namespace)
	if ns == "" {
		ns = "default"
	}
	if err := ValidateDNS1123Label(ns, "namespace"); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	ctx := r.Context()
	if h.k8sClient != nil {
		if err := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "get", ns, "environments"); err != nil {
			writeJSONError(w, http.StatusForbidden, "permission denied")
			return
		}
		if strings.TrimSpace(req.BaseURL) != "" || strings.TrimSpace(req.PreviewURL) != "" {
			if err := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "update", ns, "environments"); err != nil {
				writeJSONError(w, http.StatusForbidden, "permission denied for custom target URL override")
				return
			}
		}
	}

	var env divergeiov1alpha1.Environment
	if err := h.client.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &env); err != nil {
		if apierrors.IsNotFound(err) {
			writeJSONError(w, http.StatusNotFound, fmt.Sprintf("environment %s not found in namespace %s", envName, ns))
			return
		}
		writeJSONError(w, http.StatusInternalServerError, "failed to look up environment")
		return
	}

	var entries []replay.RequestEntry
	if strings.TrimSpace(req.TraceContent) != "" {
		formatHint := strings.TrimSpace(req.TraceFormat)
		if formatHint == "" {
			formatHint = ".jsonl"
		}
		parsedEntries, parseErr := replay.ParseTrace([]byte(req.TraceContent), formatHint)
		if parseErr != nil {
			writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("invalid traceContent: %v", parseErr))
			return
		}
		entries = parsedEntries
	} else if len(req.Requests) > 0 {
		entries = req.Requests
	} else {
		entries = []replay.RequestEntry{{ID: "default-root", Method: "GET", Path: "/"}}
	}

	allowedHosts := collectAllowedEnvHosts(&env)
	baseURL, previewURL, headerKey, headerValue, subdomainRouted := resolveServerEnvRouting(&env, req.BaseURL, req.PreviewURL)
	if err := validateServerTargetHost(baseURL, allowedHosts); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if err := validateServerTargetHost(previewURL, allowedHosts); err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if subdomainRouted && previewURL == "" {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("environment %s uses subdomain routing but has no preview URL", envName))
		return
	}
	if baseURL == "" && !allServerEntriesAbsolute(entries) {
		writeJSONError(w, http.StatusBadRequest, fmt.Sprintf("environment %s requires a baseline URL (provide baseUrl or configure spec.routing.externalUrl)", envName))
		return
	}
	for _, entry := range entries {
		for _, candidate := range []string{entry.Path, entry.URL} {
			trimmed := strings.TrimSpace(candidate)
			if strings.HasPrefix(strings.ToLower(trimmed), "http://") || strings.HasPrefix(strings.ToLower(trimmed), "https://") {
				if err := validateServerTargetHost(trimmed, allowedHosts); err != nil {
					writeJSONError(w, http.StatusBadRequest, err.Error())
					return
				}
			}
		}
	}

	stripAuth := true
	if req.StripAuth != nil {
		stripAuth = *req.StripAuth
	}

	var maxLatency time.Duration
	if req.MaxLatencyMs > 0 {
		maxLatency = time.Duration(req.MaxLatencyMs) * time.Millisecond
	}

	effectiveFilterHosts := intersectAllowedHostPatterns(req.FilterHosts, allowedHosts)

	runner := replay.NewRunner(replay.Config{
		Environment:        envName,
		Namespace:          ns,
		BaseURL:            baseURL,
		PreviewBaseURL:     previewURL,
		HeaderKey:          headerKey,
		HeaderValue:        headerValue,
		Sequential:         req.Sequential,
		MaxLatency:         maxLatency,
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		FilterHosts:        effectiveFilterHosts,
		StripHeaders:       req.StripHeaders,
		StripAuth:          stripAuth,
		RedactQueryParams:  req.RedactQueryParams,
		RedactBodyFields:   req.RedactBodyFields,
		DiffOptions: replay.DiffOptions{
			CompareValues:     req.CompareValues,
			FailOnExtraFields: req.FailOnExtraFields,
			IgnoreFields:      req.IgnoreFields,
		},
		Timeout:    15 * time.Second,
		HTTPClient: h.httpClient,
	})

	report, err := runner.Run(ctx, entries)
	if err != nil {
		writeJSONError(w, http.StatusBadRequest, err.Error())
		return
	}

	var mdBuf bytes.Buffer
	_ = replay.FormatMarkdown(&mdBuf, report)

	out := ReplayEnvironmentResponse{
		Report:   report,
		Markdown: mdBuf.String(),
	}

	if !report.Passed && req.AutoFix {
		if taskName, fixErr := h.createOrUpdateAutoFixTask(ctx, &env, "replay", mdBuf.String()); fixErr != nil {
			h.logger.Warn("auto-fix task creation failed", "environment", envName, "namespace", ns, "error", fixErr)
			out.AutoFixError = fixErr.Error()
		} else {
			out.AutoFixTaskName = taskName
		}
	}

	writeJSONResponse(w, http.StatusOK, out)
}

// resolveServerEnvRouting resolves baseline URL, preview URL, header key/value, and subdomain mode for an Environment.
func resolveServerEnvRouting(env *divergeiov1alpha1.Environment, explicitBaseURL, explicitPreviewURL string) (baseURL, previewURL, headerKey, headerValue string, subdomainRouted bool) {
	subdomainRouted = strings.EqualFold(strings.TrimSpace(env.Spec.Routing.Mode), "subdomain")
	headerKey = replay.DefaultHeaderKey
	if env.Spec.Routing.HeaderKey != "" {
		headerKey = env.Spec.Routing.HeaderKey
	}
	headerValue = env.Name
	if env.Spec.Routing.HeaderValue != "" {
		headerValue = env.Spec.Routing.HeaderValue
	}

	statusClean := stripServerPreviewQuery(env.Status.URL)
	extClean := ""
	if ext := strings.TrimSpace(env.Spec.Routing.ExternalURL); ext != "" && !strings.Contains(ext, "{") {
		extClean = stripServerPreviewQuery(ext)
	}
	envDefaultBase := stripServerPreviewQuery(os.Getenv("DIVERGE_BASE_URL"))

	baseURL = strings.TrimSpace(explicitBaseURL)
	previewURL = strings.TrimSpace(explicitPreviewURL)

	if subdomainRouted {
		if previewURL == "" {
			previewURL = statusClean
		}
		if baseURL == "" {
			if extClean != "" && !strings.EqualFold(extClean, previewURL) {
				baseURL = extClean
			} else if envDefaultBase != "" {
				baseURL = envDefaultBase
			}
		}
		return baseURL, previewURL, headerKey, headerValue, true
	}

	if baseURL == "" {
		if statusClean != "" {
			baseURL = statusClean
		} else if extClean != "" {
			baseURL = extClean
		} else if envDefaultBase != "" {
			baseURL = envDefaultBase
		}
	}
	if previewURL == "" {
		previewURL = baseURL
	}
	return baseURL, previewURL, headerKey, headerValue, false
}

// stripServerPreviewQuery strips any ?_diverge_env= parameter from rawURL.
func stripServerPreviewQuery(rawURL string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return ""
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return trimmed
	}
	q := parsed.Query()
	q.Del("_diverge_env")
	parsed.RawQuery = q.Encode()
	return strings.TrimRight(parsed.String(), "/")
}

// allServerEntriesAbsolute reports whether every entry in entries uses an absolute http:// or https:// URL.
func allServerEntriesAbsolute(entries []replay.RequestEntry) bool {
	if len(entries) == 0 {
		return false
	}
	for _, e := range entries {
		p := strings.ToLower(strings.TrimSpace(e.Path))
		if !strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
			return false
		}
	}
	return true
}

var nonDNSChars = regexp.MustCompile(`[^a-z0-9-]+`)

// collectAllowedEnvHosts derives the set of hostnames and wildcard patterns permitted as server-side verify/replay targets for env.
func collectAllowedEnvHosts(env *divergeiov1alpha1.Environment) []string {
	seen := make(map[string]struct{})
	var out []string
	addHost := func(h string) {
		cleaned := strings.ToLower(strings.TrimSpace(h))
		if cleaned == "" {
			return
		}
		if _, exists := seen[cleaned]; !exists {
			seen[cleaned] = struct{}{}
			out = append(out, cleaned)
		}
	}
	addURLHost := func(raw string) {
		trimmed := strings.TrimSpace(raw)
		if trimmed == "" {
			return
		}
		if strings.Contains(trimmed, "{") {
			replaced := strings.ReplaceAll(strings.ReplaceAll(trimmed, "{env}", "wildcard-env"), "{service}", "wildcard-svc")
			if parsed, err := url.Parse(replaced); err == nil && parsed.Hostname() != "" {
				host := strings.ToLower(parsed.Hostname())
				if idx := strings.IndexByte(host, '.'); idx > 0 {
					addHost("*" + host[idx:])
				}
			}
			return
		}
		if parsed, err := url.Parse(trimmed); err == nil && parsed.Hostname() != "" {
			addHost(parsed.Hostname())
		}
	}

	if env != nil {
		addURLHost(env.Status.URL)
		addURLHost(env.Spec.Routing.ExternalURL)
	}
	addURLHost(os.Getenv("DIVERGE_BASE_URL"))
	if allowed := os.Getenv("DIVERGE_ALLOWED_HOSTS"); allowed != "" {
		for _, part := range strings.Split(allowed, ",") {
			p := strings.ToLower(strings.TrimSpace(part))
			if p == "" {
				continue
			}
			addHost(p)
			if p != "*" && !strings.HasPrefix(p, "*.") {
				addHost("*." + p)
			}
		}
	}
	return out
}

// validateServerTargetHost verifies that rawURL is a valid target URL and that its hostname matches allowedHosts.
func validateServerTargetHost(rawURL string, allowedHosts []string) error {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return nil
	}
	if err := loadtest.ValidateTargetURL(trimmed); err != nil {
		return err
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return fmt.Errorf("invalid target URL: %w", err)
	}
	host := strings.ToLower(parsed.Hostname())
	for _, pat := range allowedHosts {
		if pat == "*" || pat == host {
			return nil
		}
		if strings.HasPrefix(pat, "*.") && strings.HasSuffix(host, pat[1:]) && len(host) > len(pat[1:]) {
			return nil
		}
	}
	return fmt.Errorf("target URL host %q is not permitted for this environment (must match status.url, spec.routing.externalUrl, DIVERGE_BASE_URL, or DIVERGE_ALLOWED_HOSTS)", parsed.Hostname())
}

// intersectAllowedHostPatterns restricts user-supplied filterHosts to patterns permitted by allowedHosts, or returns allowedHosts when userFilter is empty.
func intersectAllowedHostPatterns(userFilter, allowedHosts []string) []string {
	if len(userFilter) == 0 {
		return allowedHosts
	}
	var kept []string
	for _, raw := range userFilter {
		for _, part := range strings.Split(raw, ",") {
			cand := strings.ToLower(strings.TrimSpace(part))
			if cand == "" {
				continue
			}
			probeHost := strings.TrimPrefix(cand, "*.")
			if validateServerTargetHost("https://"+probeHost, allowedHosts) == nil {
				kept = append(kept, cand)
			}
		}
	}
	if len(kept) == 0 {
		return allowedHosts
	}
	return kept
}

// createOrUpdateAutoFixTask creates or updates an AgentTask in env.Namespace seeded with the failure summary.
func (h *VerifyReplayHandler) createOrUpdateAutoFixTask(ctx context.Context, env *divergeiov1alpha1.Environment, failureKind, summary string) (string, error) {
	ns := env.Namespace
	if ns == "" {
		ns = "default"
	}

	proj := strings.TrimSpace(env.Spec.Source.Project)
	if proj == "" {
		return "", fmt.Errorf("environment %s has no spec.source.project; cannot determine repository for auto-fix", env.Name)
	}
	var repoURL string
	if strings.HasPrefix(proj, "http://") || strings.HasPrefix(proj, "https://") || strings.HasPrefix(proj, "git@") {
		repoURL = proj
	} else if strings.EqualFold(env.Spec.Source.Provider, "gitlab") {
		repoURL = "https://gitlab.com/" + proj
	} else {
		repoURL = "https://github.com/" + proj
	}
	branch := strings.TrimSpace(env.Spec.Source.Branch)
	if branch == "" {
		branch = env.Name
	}

	rawName := strings.ToLower(fmt.Sprintf("autofix-%s-%s", failureKind, env.Name))
	taskName := strings.Trim(nonDNSChars.ReplaceAllString(rawName, "-"), "-")
	if len(taskName) > 50 {
		taskName = strings.TrimRight(taskName[:50], "-")
	}

	objective := fmt.Sprintf("Fix %s regression detected on preview environment %s:\n\n%s", failureKind, env.Name, strings.TrimSpace(summary))

	var existing divergeiov1alpha1.AgentTask
	err := h.client.Get(ctx, client.ObjectKey{Name: taskName, Namespace: ns}, &existing)
	if err == nil {
		if h.k8sClient != nil {
			if authErr := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "update", ns, "agenttasks"); authErr != nil {
				return "", authErr
			}
		}
		if existing.Annotations == nil {
			existing.Annotations = make(map[string]string)
		}
		existing.Annotations["agent.diverge.io/guidance"] = objective
		existing.Spec.Objective = objective
		existing.Spec.Suspended = false
		if err := h.client.Update(ctx, &existing); err != nil {
			return "", err
		}
		return taskName, nil
	}
	if !apierrors.IsNotFound(err) {
		return "", err
	}

	if h.k8sClient != nil {
		if authErr := AuthorizeAction(ctx, h.k8sClient, h.auditLogger, "create", ns, "agenttasks"); authErr != nil {
			return "", authErr
		}
	}

	task := &divergeiov1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: ns,
			Labels: map[string]string{
				"diverge.io/environment": env.Name,
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
	if err := h.client.Create(ctx, task); err != nil {
		return "", err
	}
	return taskName, nil
}

// writeJSONResponse writes v as JSON with the given HTTP status code.
func writeJSONResponse(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeJSONError writes a structured JSON error payload with the given HTTP status code.
func writeJSONError(w http.ResponseWriter, status int, message string) {
	writeJSONResponse(w, status, map[string]string{"error": message})
}

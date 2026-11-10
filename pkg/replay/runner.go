package replay

import (
	"bytes"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"text/tabwriter"
	"time"

	"github.com/divergedev/diverge/pkg/loadtest"
)

const (
	DefaultConcurrency = 4
	DefaultTimeout     = 15 * time.Second
	DefaultHeaderKey   = "x-diverge-env"
	maxResponseBytes   = 10 * 1024 * 1024 // 10 MiB cap per response body
)

// Config configures a deterministic traffic replay run.
type Config struct {
	Environment        string
	Namespace          string
	BaseURL            string
	PreviewBaseURL     string
	HeaderKey          string
	HeaderValue        string
	Concurrency        int
	Sequential         bool
	Timeout            time.Duration
	MaxLatency         time.Duration
	FailOnStatusDiff   bool
	FailOnContractDiff bool
	FilterHosts        []string
	StripHeaders       []string
	StripAuth          bool
	RedactQueryParams  []string
	RedactBodyFields   []string
	DiffOptions        DiffOptions
	HTTPClient         *http.Client
}

// EntryResult records the baseline vs. preview comparison outcome for a single replayed request.
type EntryResult struct {
	Index             int               `json:"index"`
	ID                string            `json:"id,omitempty"`
	Method            string            `json:"method"`
	Path              string            `json:"path"`
	BaselineStatus    int               `json:"baselineStatus"`
	PreviewStatus     int               `json:"previewStatus"`
	BaselineLatencyMs int64             `json:"baselineLatencyMs"`
	PreviewLatencyMs  int64             `json:"previewLatencyMs"`
	Protocol          string            `json:"protocol"`
	StatusMatch       bool              `json:"statusMatch"`
	ContractDiffs     []ContractDiff    `json:"contractDiffs,omitempty"`
	BreakingDiffCount int               `json:"breakingDiffCount"`
	ExtractedVars     map[string]string `json:"extractedVars,omitempty"`
	Passed            bool              `json:"passed"`
	FailureReasons    []string          `json:"failureReasons,omitempty"`
}

// Report aggregates the results of replaying a trace against baseline and preview endpoints.
type Report struct {
	Environment      string        `json:"environment,omitempty"`
	Namespace        string        `json:"namespace,omitempty"`
	TargetURL        string        `json:"targetUrl,omitempty"`
	PreviewURL       string        `json:"previewUrl,omitempty"`
	RoutingHeader    string        `json:"routingHeader,omitempty"`
	TotalRequests    int           `json:"totalRequests"`
	PassedRequests   int           `json:"passedRequests"`
	FailedRequests   int           `json:"failedRequests"`
	StatusMismatches int           `json:"statusMismatches"`
	BreakingDiffs    int           `json:"breakingDiffs"`
	BaselineP50Ms    int64         `json:"baselineP50Ms"`
	BaselineP95Ms    int64         `json:"baselineP95Ms"`
	PreviewP50Ms     int64         `json:"previewP50Ms"`
	PreviewP95Ms     int64         `json:"previewP95Ms"`
	Passed           bool          `json:"passed"`
	Results          []EntryResult `json:"results"`
}

// Runner executes deterministic traffic replay and contract diffing.
type Runner struct {
	Config Config
}

// NewRunner creates a new traffic replay Runner.
func NewRunner(cfg Config) *Runner {
	if cfg.Concurrency <= 0 {
		cfg.Concurrency = DefaultConcurrency
	}
	if cfg.Timeout <= 0 {
		cfg.Timeout = DefaultTimeout
	}
	if strings.TrimSpace(cfg.HeaderKey) == "" {
		cfg.HeaderKey = DefaultHeaderKey
	}
	return &Runner{Config: cfg}
}

// Run replays the provided trace entries against baseline and preview endpoints.
func (r *Runner) Run(ctx context.Context, entries []RequestEntry) (*Report, error) {
	if len(r.Config.FilterHosts) > 0 {
		entries = FilterEntriesByHost(entries, r.Config.FilterHosts)
	}
	if len(entries) == 0 {
		return nil, errors.New("cannot run replay with 0 trace entries")
	}

	client := secureHTTPClient(r.Config.HTTPClient, r.Config.Timeout)

	previewBase := strings.TrimSpace(r.Config.PreviewBaseURL)
	if previewBase == "" {
		previewBase = strings.TrimSpace(r.Config.BaseURL)
	}

	results := make([]EntryResult, len(entries))
	workerCount := r.Config.Concurrency
	if r.Config.Sequential || HasChaining(entries) {
		workerCount = 1
	}
	if workerCount > len(entries) {
		workerCount = len(entries)
	}

	chainStore := newChainingStore()

	jobs := make(chan int, len(entries))
	for i := range entries {
		jobs <- i
	}
	close(jobs)

	var wg sync.WaitGroup
	for w := 0; w < workerCount; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for idx := range jobs {
				if ctx.Err() != nil {
					results[idx] = EntryResult{
						Index:          idx,
						ID:             entries[idx].ID,
						Method:         entries[idx].Method,
						Path:           SanitizeURLString(entries[idx].Path, r.Config.StripAuth, r.Config.RedactQueryParams),
						Passed:         false,
						FailureReasons: []string{ctx.Err().Error()},
					}
					continue
				}
				results[idx] = r.replayEntry(ctx, client, idx, entries[idx], r.Config.BaseURL, previewBase, chainStore)
			}
		}()
	}
	wg.Wait()

	report := &Report{
		Environment:   r.Config.Environment,
		Namespace:     r.Config.Namespace,
		TargetURL:     r.Config.BaseURL,
		PreviewURL:    previewBase,
		TotalRequests: len(entries),
		Results:       results,
	}
	if r.Config.HeaderKey != "" && r.Config.HeaderValue != "" {
		report.RoutingHeader = fmt.Sprintf("%s: %s", r.Config.HeaderKey, r.Config.HeaderValue)
	}

	baseLatencies := make([]int64, 0, len(results))
	prevLatencies := make([]int64, 0, len(results))

	for _, res := range results {
		if res.Passed {
			report.PassedRequests++
		} else {
			report.FailedRequests++
		}
		if !res.StatusMatch {
			report.StatusMismatches++
		}
		report.BreakingDiffs += res.BreakingDiffCount
		if res.BaselineStatus > 0 {
			baseLatencies = append(baseLatencies, res.BaselineLatencyMs)
		}
		if res.PreviewStatus > 0 {
			prevLatencies = append(prevLatencies, res.PreviewLatencyMs)
		}
	}

	report.BaselineP50Ms = percentileInt64(baseLatencies, 0.50)
	report.BaselineP95Ms = percentileInt64(baseLatencies, 0.95)
	report.PreviewP50Ms = percentileInt64(prevLatencies, 0.50)
	report.PreviewP95Ms = percentileInt64(prevLatencies, 0.95)
	report.Passed = report.TotalRequests > 0 && report.FailedRequests == 0

	return report, nil
}

type requestSanitizePolicy struct {
	routingKey           string
	routingVal           string
	stripAuth            bool
	stripHeaders         []string
	redactQueryParams    []string
	noFollowRedirect     bool
	allowedRedirectHosts []string
}

type sanitizePolicyContextKey struct{}

type sanitizingRoundTripper struct {
	base http.RoundTripper
}

// RoundTrip sanitizes outbound request headers, query parameters, and cookies (including any cookies injected by an http.CookieJar) before delegating to the underlying transport.
func (s *sanitizingRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if policy, ok := req.Context().Value(sanitizePolicyContextKey{}).(requestSanitizePolicy); ok {
		cloned := req.Clone(req.Context())
		sanitizeRequestHeaders(cloned, policy.routingKey, policy.routingVal, policy.stripAuth, policy.stripHeaders)
		sanitizeURLObject(cloned.URL, policy.stripAuth, policy.redactQueryParams)
		req = cloned
	}
	return s.base.RoundTrip(req)
}

// secureHTTPClient returns an http.Client with dial-time SSRF IP validation and redirect target validation.
func secureHTTPClient(baseClient *http.Client, timeout time.Duration) *http.Client {
	dialer := &net.Dialer{
		Timeout:   30 * time.Second,
		KeepAlive: 30 * time.Second,
	}

	safeDial := func(ctx context.Context, network, addr string, innerDial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
		host, port, err := net.SplitHostPort(addr)
		if err != nil {
			return nil, fmt.Errorf("splitting host and port %q: %w", addr, err)
		}
		if loadtest.IsProhibitedHost(host) {
			return nil, fmt.Errorf("connection to prohibited destination %s blocked", host)
		}

		var ips []net.IP
		if ip := net.ParseIP(host); ip != nil {
			ips = []net.IP{ip}
		} else {
			resolvedIPs, err := net.DefaultResolver.LookupIP(ctx, "ip", host)
			if err != nil {
				return nil, fmt.Errorf("resolving host %q: %w", host, err)
			}
			ips = resolvedIPs
		}
		if len(ips) == 0 {
			return nil, fmt.Errorf("no IP addresses found for host %s", host)
		}
		for _, ip := range ips {
			if loadtest.IsProhibitedIP(ip) {
				return nil, fmt.Errorf("connection to prohibited IP %s (%s) blocked", ip.String(), host)
			}
		}

		var lastErr error
		for _, ip := range ips {
			targetAddr := net.JoinHostPort(ip.String(), port)
			var conn net.Conn
			var dialErr error
			if innerDial != nil {
				conn, dialErr = innerDial(ctx, network, targetAddr)
			} else {
				conn, dialErr = dialer.DialContext(ctx, network, targetAddr)
			}
			if dialErr == nil {
				return conn, nil
			}
			lastErr = dialErr
		}
		return nil, lastErr
	}

	var out http.Client
	if baseClient != nil {
		out = *baseClient
	} else {
		out = http.Client{Timeout: timeout}
	}

	var baseTransport http.RoundTripper
	switch tr := out.Transport.(type) {
	case nil:
		baseTransport = &http.Transport{
			ForceAttemptHTTP2:   true,
			MaxIdleConns:        200,
			MaxIdleConnsPerHost: 50,
			IdleConnTimeout:     90 * time.Second,
			TLSClientConfig: &tls.Config{
				MinVersion: tls.VersionTLS12,
			},
			DialContext: func(ctx context.Context, network, addr string) (net.Conn, error) {
				return safeDial(ctx, network, addr, nil)
			},
		}
	case *http.Transport:
		cloned := tr.Clone()
		origDial := cloned.DialContext
		cloned.DialContext = func(ctx context.Context, network, addr string) (net.Conn, error) {
			return safeDial(ctx, network, addr, origDial)
		}
		baseTransport = cloned
	default:
		baseTransport = tr
	}
	out.Transport = &sanitizingRoundTripper{base: baseTransport}

	origRedirect := out.CheckRedirect
	out.CheckRedirect = func(req *http.Request, via []*http.Request) error {
		if policy, ok := req.Context().Value(sanitizePolicyContextKey{}).(requestSanitizePolicy); ok {
			if policy.noFollowRedirect {
				return http.ErrUseLastResponse
			}
			if len(policy.allowedRedirectHosts) > 0 && !hostMatchesFilter(strings.ToLower(req.URL.Hostname()), normalizeHostPatterns(policy.allowedRedirectHosts)) {
				return fmt.Errorf("redirect target host %q not in allowed hosts", req.URL.Hostname())
			}
			sanitizeRequestHeaders(req, policy.routingKey, policy.routingVal, policy.stripAuth, policy.stripHeaders)
			sanitizeURLObject(req.URL, policy.stripAuth, policy.redactQueryParams)
		}
		if err := loadtest.ValidateTargetURL(req.URL.String()); err != nil {
			return fmt.Errorf("redirect target prohibited: %w", err)
		}
		if origRedirect != nil {
			return origRedirect(req, via)
		}
		if len(via) >= 10 {
			return errors.New("stopped after 10 redirects")
		}
		return nil
	}

	return &out
}

// replayEntry executes a single trace entry against baseline and preview endpoints and compares their responses.
func (r *Runner) replayEntry(ctx context.Context, client *http.Client, index int, entry RequestEntry, baseURL, previewBaseURL string, chainStore *chainingStore) EntryResult {
	baseVars, prevVars := chainStore.snapshot()
	interpBasePath := InterpolateString(entry.Path, baseVars)
	interpPrevPath := InterpolateString(entry.Path, prevVars)
	sanitizedBasePath := SanitizeURLString(interpBasePath, r.Config.StripAuth, r.Config.RedactQueryParams)
	sanitizedPrevPath := SanitizeURLString(interpPrevPath, r.Config.StripAuth, r.Config.RedactQueryParams)

	res := EntryResult{
		Index:  index,
		ID:     entry.ID,
		Method: entry.Method,
		Path:   sanitizedPrevPath,
		Passed: true,
	}

	payload, err := entry.PayloadBytes()
	if err != nil {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, err.Error())
		return res
	}
	basePayload := InterpolateBytes(payload, baseVars)
	prevPayload := InterpolateBytes(payload, prevVars)
	if len(r.Config.RedactBodyFields) > 0 {
		if len(basePayload) > 0 {
			if redacted, ok := RedactJSONBodyFields(basePayload, r.Config.RedactBodyFields); ok {
				basePayload = redacted
			}
		}
		if len(prevPayload) > 0 {
			if redacted, ok := RedactJSONBodyFields(prevPayload, r.Config.RedactBodyFields); ok {
				prevPayload = redacted
			}
		}
	}

	resolvedBaseURL, err := ResolveEntryURL(baseURL, sanitizedBasePath)
	if err != nil {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("resolve baseline URL: %v", err))
		return res
	}
	resolvedPrevURL, err := ResolveEntryURL(previewBaseURL, sanitizedPrevPath)
	if err != nil {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("resolve preview URL: %v", err))
		return res
	}

	baseHeaders := InterpolateHeaders(entry.Headers, baseVars)
	prevHeaders := InterpolateHeaders(entry.Headers, prevVars)

	noFollowRedirect := entry.ExpectStatus >= 300 && entry.ExpectStatus < 400
	baseStatus, baseCT, baseGRPCStatus, baseRespHeaders, baseBody, baseDur, baseErr := executeHTTP(ctx, client, r.Config.Timeout, entry.Method, resolvedBaseURL, baseHeaders, basePayload, r.Config.HeaderKey, "", r.Config.StripAuth, r.Config.StripHeaders, r.Config.RedactQueryParams, noFollowRedirect, r.Config.FilterHosts)
	res.BaselineLatencyMs = baseDur.Milliseconds()
	if baseErr != nil {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("baseline request failed: %v", baseErr))
		return res
	}
	res.BaselineStatus = baseStatus

	prevStatus, prevCT, prevGRPCStatus, prevRespHeaders, prevBody, prevDur, prevErr := executeHTTP(ctx, client, r.Config.Timeout, entry.Method, resolvedPrevURL, prevHeaders, prevPayload, r.Config.HeaderKey, r.Config.HeaderValue, r.Config.StripAuth, r.Config.StripHeaders, r.Config.RedactQueryParams, noFollowRedirect, r.Config.FilterHosts)
	res.PreviewLatencyMs = prevDur.Milliseconds()
	if prevErr != nil {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("preview request failed: %v", prevErr))
		return res
	}
	res.PreviewStatus = prevStatus

	if len(entry.Extract) > 0 {
		extractedBase, baseExtErr := ExtractResponseVars(entry.Extract, baseStatus, baseRespHeaders, baseBody)
		extractedPrev, prevExtErr := ExtractResponseVars(entry.Extract, prevStatus, prevRespHeaders, prevBody)
		if baseExtErr != nil {
			res.Passed = false
			res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("baseline variable extraction failed: %v", baseExtErr))
		}
		if prevExtErr != nil {
			res.Passed = false
			res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("preview variable extraction failed: %v", prevExtErr))
		}
		if baseExtErr == nil && prevExtErr == nil {
			chainStore.merge(extractedBase, extractedPrev)
			res.ExtractedVars = extractedPrev
		}
	}

	res.StatusMatch = baseStatus == prevStatus
	if !res.StatusMatch && r.Config.FailOnStatusDiff {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("status mismatch: baseline HTTP %d vs preview HTTP %d", baseStatus, prevStatus))
	}
	if entry.ExpectStatus > 0 && prevStatus != entry.ExpectStatus {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("expected HTTP %d, got %d", entry.ExpectStatus, prevStatus))
	}

	if r.Config.MaxLatency > 0 && prevDur > r.Config.MaxLatency {
		res.Passed = false
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("preview latency %s exceeds max-latency %s", prevDur.Round(time.Millisecond), r.Config.MaxLatency))
	}

	diffOpts := r.Config.DiffOptions
	if entry.ProtoMessage != "" {
		diffOpts.ProtoMessage = entry.ProtoMessage
	}

	protocol, diffs := ComparePayloads(baseCT, prevCT, baseBody, prevBody, diffOpts)
	if baseGRPCStatus != "" || prevGRPCStatus != "" {
		normBase := normalizeGRPCStatus(baseGRPCStatus)
		normPrev := normalizeGRPCStatus(prevGRPCStatus)
		if normBase != normPrev {
			res.StatusMatch = false
			if r.Config.FailOnStatusDiff {
				res.Passed = false
				res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("gRPC status mismatch: baseline grpc-status=%s vs preview grpc-status=%s", normBase, normPrev))
			}
			diffs = append(diffs, ContractDiff{
				Path:     "grpc.status",
				Kind:     "status_mismatch",
				Baseline: normBase,
				Preview:  normPrev,
				Breaking: true,
			})
		}
	}

	res.Protocol = protocol
	res.ContractDiffs = diffs

	breakingCount := 0
	for _, d := range diffs {
		if d.Breaking {
			breakingCount++
		}
	}
	res.BreakingDiffCount = breakingCount

	if breakingCount > 0 && r.Config.FailOnContractDiff {
		res.Passed = false
		first := diffs[0]
		for _, d := range diffs {
			if d.Breaking {
				first = d
				break
			}
		}
		res.FailureReasons = append(res.FailureReasons, fmt.Sprintf("%d breaking contract diff(s) (e.g. %s: %s)", breakingCount, first.Path, first.Kind))
	}

	return res
}

// normalizeGRPCStatus normalizes an empty Grpc-Status header/trailer value to "0" (OK).
func normalizeGRPCStatus(status string) string {
	trimmed := strings.TrimSpace(status)
	if trimmed == "" {
		return "0"
	}
	return trimmed
}

// hasHTTPScheme reports whether raw starts with a case-insensitive http:// or https:// scheme.
func hasHTTPScheme(raw string) bool {
	return (len(raw) >= 7 && strings.EqualFold(raw[:7], "http://")) ||
		(len(raw) >= 8 && strings.EqualFold(raw[:8], "https://"))
}

// ResolveEntryURL resolves a trace entry's path or URL against an optional base URL and validates against SSRF.
func ResolveEntryURL(baseURL, entryPath string) (string, error) {
	trimmedBase := strings.TrimSpace(baseURL)
	trimmedPath := strings.TrimSpace(entryPath)

	var finalURL string
	if hasHTTPScheme(trimmedPath) {
		if trimmedBase == "" {
			finalURL = trimmedPath
		} else {
			parsedEntry, err := url.Parse(trimmedPath)
			if err != nil {
				return "", fmt.Errorf("invalid entry URL %q: %w", trimmedPath, err)
			}
			parsedBase, err := url.Parse(trimmedBase)
			if err != nil {
				return "", fmt.Errorf("invalid base URL %q: %w", trimmedBase, err)
			}
			rewritten := *parsedEntry
			rewritten.Scheme = parsedBase.Scheme
			rewritten.Host = parsedBase.Host
			if basePrefix := strings.TrimRight(parsedBase.Path, "/"); basePrefix != "" {
				entryRel := parsedEntry.Path
				if !strings.HasPrefix(entryRel, "/") {
					entryRel = "/" + entryRel
				}
				rewritten.Path = basePrefix + entryRel
			}
			finalURL = rewritten.String()
		}
	} else {
		if trimmedBase == "" {
			return "", fmt.Errorf("base URL is required for relative path %q", trimmedPath)
		}
		rel := trimmedPath
		if !strings.HasPrefix(rel, "/") {
			rel = "/" + rel
		}
		finalURL = strings.TrimRight(trimmedBase, "/") + rel
	}

	if err := loadtest.ValidateTargetURL(finalURL); err != nil {
		return "", err
	}
	return finalURL, nil
}

// normalizeHostPatterns splits comma-separated host patterns and normalizes them to lowercase.
func normalizeHostPatterns(allowedHosts []string) []string {
	var normalized []string
	for _, h := range allowedHosts {
		for _, part := range strings.Split(h, ",") {
			cleaned := strings.ToLower(strings.TrimSpace(part))
			if cleaned != "" {
				normalized = append(normalized, cleaned)
			}
		}
	}
	return normalized
}

// FilterEntriesByHost retains relative-path entries and absolute-URL entries whose hostname matches one of allowedHosts
// (supporting exact hostname matches like "api.example.com" and wildcard subdomain patterns like "*.example.com").
func FilterEntriesByHost(entries []RequestEntry, allowedHosts []string) []RequestEntry {
	if len(allowedHosts) == 0 {
		return entries
	}
	normalized := normalizeHostPatterns(allowedHosts)
	if len(normalized) == 0 {
		return []RequestEntry{}
	}

	filtered := make([]RequestEntry, 0, len(entries))
	for _, entry := range entries {
		trimmed := strings.TrimSpace(entry.Path)
		if !hasHTTPScheme(trimmed) {
			filtered = append(filtered, entry)
			continue
		}
		parsed, err := url.Parse(trimmed)
		if err != nil {
			continue
		}
		host := strings.ToLower(parsed.Hostname())
		if hostMatchesFilter(host, normalized) {
			filtered = append(filtered, entry)
		}
	}
	return filtered
}

// hostMatchesFilter reports whether host matches any exact hostname or "*.domain" wildcard pattern in patterns.
func hostMatchesFilter(host string, patterns []string) bool {
	if host == "" {
		return false
	}
	for _, pat := range patterns {
		if host == pat {
			return true
		}
		if strings.HasPrefix(pat, "*.") {
			suffix := pat[1:] // ".example.com"
			if strings.HasSuffix(host, suffix) && len(host) > len(suffix) {
				return true
			}
		}
	}
	return false
}

// sanitizeRequestHeaders strips configured auth/custom headers and recorded Diverge routing headers/cookies, then injects preview routing headers when routingVal is set.
func sanitizeRequestHeaders(req *http.Request, routingKey, routingVal string, stripAuth bool, stripHeaders []string) {
	if stripAuth {
		sanitizeURLObject(req.URL, true, nil)
		req.Header.Del("Authorization")
		req.Header.Del("Proxy-Authorization")
		req.Header.Del("Cookie")
		req.Header.Del("X-Api-Key")
	}
	for _, h := range stripHeaders {
		if trimmed := strings.TrimSpace(h); trimmed != "" {
			req.Header.Del(trimmed)
		}
	}

	// Always strip any recorded routing headers, diverge_env sticky-routing cookies, or diverge.env baggage items first
	// so baseline requests never route to preview and preview requests route strictly to routingVal.
	req.Header.Del(DefaultHeaderKey)
	if routingKey != "" {
		req.Header.Del(routingKey)
	}
	stripDivergeBaggage(&req.Header)
	stripDivergeCookie(&req.Header)

	if routingKey != "" && routingVal != "" {
		req.Header.Set(routingKey, routingVal)
		baggageItem := fmt.Sprintf("diverge.env=%s", routingVal)
		if existing := req.Header.Get("baggage"); existing != "" {
			req.Header.Set("baggage", existing+","+baggageItem)
		} else {
			req.Header.Set("baggage", baggageItem)
		}
	}
}

// executeHTTP performs a single HTTP request, enforcing per-request timeout, routing headers, body size bounds, and gRPC trailer extraction.
func executeHTTP(ctx context.Context, client *http.Client, timeout time.Duration, method, targetURL string, headers map[string]string, body []byte, routingKey, routingVal string, stripAuth bool, stripHeaders []string, redactQueryParams []string, noFollowRedirect bool, allowedRedirectHosts []string) (int, string, string, http.Header, []byte, time.Duration, error) {
	policy := requestSanitizePolicy{
		routingKey:           routingKey,
		routingVal:           routingVal,
		stripAuth:            stripAuth,
		stripHeaders:         stripHeaders,
		redactQueryParams:    redactQueryParams,
		noFollowRedirect:     noFollowRedirect,
		allowedRedirectHosts: allowedRedirectHosts,
	}
	reqCtx := context.WithValue(ctx, sanitizePolicyContextKey{}, policy)
	var cancel context.CancelFunc
	if timeout > 0 {
		reqCtx, cancel = context.WithTimeout(reqCtx, timeout)
		defer cancel()
	}

	var bodyReader io.Reader
	if len(body) > 0 {
		bodyReader = bytes.NewReader(body)
	}

	req, err := http.NewRequestWithContext(reqCtx, method, targetURL, bodyReader)
	if err != nil {
		return 0, "", "", nil, nil, 0, err
	}

	for k, v := range headers {
		req.Header.Set(k, v)
	}

	sanitizeRequestHeaders(req, routingKey, routingVal, stripAuth, stripHeaders)
	sanitizeURLObject(req.URL, stripAuth, redactQueryParams)

	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", "", nil, nil, time.Since(start), err
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, err := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes+1))
	dur := time.Since(start)
	if err != nil {
		return resp.StatusCode, resp.Header.Get("Content-Type"), "", resp.Header, nil, dur, fmt.Errorf("read response body: %w", err)
	}
	if len(respBody) > maxResponseBytes {
		return resp.StatusCode, resp.Header.Get("Content-Type"), "", resp.Header, nil, dur, fmt.Errorf("response body exceeds maximum comparison size of %d bytes", maxResponseBytes)
	}

	grpcStatus := strings.TrimSpace(resp.Header.Get("Grpc-Status"))
	if grpcStatus == "" {
		grpcStatus = strings.TrimSpace(resp.Trailer.Get("Grpc-Status"))
	}

	return resp.StatusCode, resp.Header.Get("Content-Type"), grpcStatus, resp.Header, respBody, dur, nil
}

// stripDivergeBaggage removes any diverge.env=<value> entries from the W3C baggage header.
func stripDivergeBaggage(hdr *http.Header) {
	existing := hdr.Get("baggage")
	if existing == "" {
		return
	}
	parts := strings.Split(existing, ",")
	kept := make([]string, 0, len(parts))
	for _, p := range parts {
		trimmed := strings.TrimSpace(p)
		if trimmed == "" || strings.HasPrefix(strings.ToLower(trimmed), "diverge.env=") {
			continue
		}
		kept = append(kept, trimmed)
	}
	if len(kept) == 0 {
		hdr.Del("baggage")
	} else {
		hdr.Set("baggage", strings.Join(kept, ","))
	}
}

// stripDivergeCookie removes any recorded diverge_env=<value> sticky-routing cookies from Cookie headers.
func stripDivergeCookie(hdr *http.Header) {
	vals := hdr.Values("Cookie")
	if len(vals) == 0 {
		return
	}
	var newVals []string
	for _, rawVal := range vals {
		parts := strings.Split(rawVal, ";")
		kept := make([]string, 0, len(parts))
		for _, p := range parts {
			trimmed := strings.TrimSpace(p)
			if trimmed == "" {
				continue
			}
			name, _, _ := strings.Cut(trimmed, "=")
			if strings.TrimSpace(name) == "diverge_env" {
				continue
			}
			kept = append(kept, trimmed)
		}
		if len(kept) > 0 {
			newVals = append(newVals, strings.Join(kept, "; "))
		}
	}
	hdr.Del("Cookie")
	for _, v := range newVals {
		hdr.Add("Cookie", v)
	}
}

// percentileInt64 computes the given percentile (0.0 to 1.0) of a slice of int64 values.
func percentileInt64(values []int64, pct float64) int64 {
	if len(values) == 0 {
		return 0
	}
	sorted := make([]int64, len(values))
	copy(sorted, values)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i] < sorted[j] })
	idx := int(float64(len(sorted)-1) * pct)
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

// FormatJSON writes the replay report as indented JSON.
func FormatJSON(w io.Writer, report *Report) error {
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

// formatExtractedVars formats extracted variable key-value pairs in deterministic sorted order.
func formatExtractedVars(vars map[string]string) string {
	if len(vars) == 0 {
		return ""
	}
	keys := make([]string, 0, len(vars))
	for k := range vars {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, k := range keys {
		parts = append(parts, fmt.Sprintf("%s=%s", k, vars[k]))
	}
	return "extracted: " + strings.Join(parts, ", ")
}

// FormatTable renders a human-readable tabular summary of the replay report.
func FormatTable(w io.Writer, report *Report) {
	tw := tabwriter.NewWriter(w, 0, 0, 2, ' ', 0)
	_, _ = fmt.Fprintln(tw, "ID\tMETHOD\tPATH\tPROTO\tSTATUS (B->P)\tLATENCY (B->P)\tRESULT\tDETAILS")
	for _, r := range report.Results {
		result := "[PASS]"
		if !r.Passed {
			result = "[FAIL]"
		}
		statusStr := fmt.Sprintf("%d -> %d", r.BaselineStatus, r.PreviewStatus)
		latStr := fmt.Sprintf("%dms -> %dms", r.BaselineLatencyMs, r.PreviewLatencyMs)
		protoStr := r.Protocol
		if protoStr == "" {
			protoStr = "-"
		}
		details := "parity ok"
		if len(r.FailureReasons) > 0 {
			details = strings.Join(r.FailureReasons, "; ")
		} else if len(r.ContractDiffs) > 0 {
			details = fmt.Sprintf("%d non-breaking diff(s)", len(r.ContractDiffs))
		}
		if extSummary := formatExtractedVars(r.ExtractedVars); extSummary != "" {
			details = fmt.Sprintf("%s (%s)", details, extSummary)
		}
		_, _ = fmt.Fprintf(tw, "%s\t%s\t%s\t%s\t%s\t%s\t%s\t%s\n",
			r.ID, r.Method, r.Path, protoStr, statusStr, latStr, result, details)
	}
	_ = tw.Flush()

	_, _ = fmt.Fprintln(w)
	_, _ = fmt.Fprintf(w, "Latency p50 (baseline -> preview): %dms -> %dms | p95: %dms -> %dms\n",
		report.BaselineP50Ms, report.PreviewP50Ms, report.BaselineP95Ms, report.PreviewP95Ms)
	if report.Passed {
		_, _ = fmt.Fprintf(w, "Replay PASSED (%d/%d requests matched contract)\n", report.PassedRequests, report.TotalRequests)
	} else {
		_, _ = fmt.Fprintf(w, "Replay FAILED (%d/%d requests failed, %d status mismatches, %d breaking contract diffs)\n",
			report.FailedRequests, report.TotalRequests, report.StatusMismatches, report.BreakingDiffs)
	}
}

// FormatMarkdown renders a GitHub Step Summary Markdown report for the replay run.
func FormatMarkdown(w io.Writer, report *Report) error {
	var buf bytes.Buffer
	envLabel := report.Environment
	if envLabel == "" {
		envLabel = report.TargetURL
	}
	if report.Passed {
		_, _ = fmt.Fprintf(&buf, "### ✅ Diverge Traffic Replay Passed: %s\n\n", envLabel)
	} else {
		_, _ = fmt.Fprintf(&buf, "### ❌ Diverge Traffic Replay Failed: %s\n\n", envLabel)
	}

	targetStr := report.TargetURL
	if targetStr == "" {
		targetStr = "-"
	}
	headerStr := report.RoutingHeader
	if headerStr == "" {
		headerStr = "-"
	}
	_, _ = fmt.Fprintf(&buf, "**Target:** %s | **Header:** `%s` | **Requests:** %d (%d passed, %d failed) | **p95 Latency:** %dms → %dms\n\n",
		targetStr, headerStr, report.TotalRequests, report.PassedRequests, report.FailedRequests, report.BaselineP95Ms, report.PreviewP95Ms)

	_, _ = fmt.Fprintln(&buf, "| ID | Method | Path | Protocol | Status (Base → Prev) | Latency (Base → Prev) | Result | Details |")
	_, _ = fmt.Fprintln(&buf, "| :--- | :--- | :--- | :--- | :--- | :--- | :--- | :--- |")
	for _, r := range report.Results {
		result := "✅ PASS"
		if !r.Passed {
			result = "❌ FAIL"
		}
		protoStr := r.Protocol
		if protoStr == "" {
			protoStr = "-"
		}
		details := "Parity OK"
		if len(r.FailureReasons) > 0 {
			details = strings.Join(r.FailureReasons, "; ")
		} else if len(r.ContractDiffs) > 0 {
			details = fmt.Sprintf("%d non-breaking diff(s)", len(r.ContractDiffs))
		}
		if extSummary := formatExtractedVars(r.ExtractedVars); extSummary != "" {
			details = fmt.Sprintf("%s (%s)", details, extSummary)
		}
		details = strings.ReplaceAll(details, "|", "\\|")
		pathEsc := strings.ReplaceAll(r.Path, "|", "\\|")
		_, _ = fmt.Fprintf(&buf, "| %s | %s | `%s` | `%s` | %d → %d | %dms → %dms | %s | %s |\n",
			r.ID, r.Method, pathEsc, protoStr, r.BaselineStatus, r.PreviewStatus, r.BaselineLatencyMs, r.PreviewLatencyMs, result, details)
	}

	hasDiffs := false
	for _, r := range report.Results {
		if len(r.ContractDiffs) > 0 {
			hasDiffs = true
			break
		}
	}
	if hasDiffs {
		_, _ = fmt.Fprintln(&buf, "\n#### 🔍 Contract & Wire Diffs")
		_, _ = fmt.Fprintln(&buf, "| Request | Field Path | Kind | Baseline | Preview | Breaking |")
		_, _ = fmt.Fprintln(&buf, "| :--- | :--- | :--- | :--- | :--- | :--- |")
		for _, r := range report.Results {
			for _, d := range r.ContractDiffs {
				brk := "No"
				if d.Breaking {
					brk = "**Yes**"
				}
				rPathEsc := strings.ReplaceAll(r.Path, "|", "\\|")
				dPathEsc := strings.ReplaceAll(d.Path, "|", "\\|")
				bEsc := strings.ReplaceAll(d.Baseline, "|", "\\|")
				pEsc := strings.ReplaceAll(d.Preview, "|", "\\|")
				_, _ = fmt.Fprintf(&buf, "| %s (`%s`) | `%s` | `%s` | `%s` | `%s` | %s |\n",
					r.ID, rPathEsc, dPathEsc, d.Kind, bEsc, pEsc, brk)
			}
		}
	}

	_, err := w.Write(buf.Bytes())
	return err
}

package replay

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/divergedev/diverge/pkg/loadtest"
)

// RedactedPlaceholder is the replacement string used when redacting sensitive query parameters or JSON body fields.
const RedactedPlaceholder = "[REDACTED]"

// defaultAuthQueryParams lists lowercase query parameter names that carry credentials or signatures
// and are automatically redacted when StripAuth is enabled.
var defaultAuthQueryParams = map[string]struct{}{
	"token":                {},
	"access_token":         {},
	"id_token":             {},
	"refresh_token":        {},
	"auth":                 {},
	"auth_token":           {},
	"api_key":              {},
	"apikey":               {},
	"key":                  {},
	"secret":               {},
	"client_secret":        {},
	"password":             {},
	"passwd":               {},
	"sig":                  {},
	"signature":            {},
	"x-amz-signature":      {},
	"x-amz-security-token": {},
	"x-goog-signature":     {},
}

// SanitizeOptions configures trace and live-request credential and PII sanitization.
type SanitizeOptions struct {
	StripAuth         bool
	StripHeaders      []string
	RedactQueryParams []string
	RedactBodyFields  []string
	FilterHosts       []string
}

// SanitizeEntries filters and sanitizes a slice of RequestEntry items according to opts,
// returning a new slice ready for safe replay or persistence to disk.
func SanitizeEntries(entries []RequestEntry, opts SanitizeOptions) []RequestEntry {
	working := entries
	if len(opts.FilterHosts) > 0 {
		working = FilterEntriesByHost(working, opts.FilterHosts)
	}
	if len(working) == 0 {
		return []RequestEntry{}
	}
	out := make([]RequestEntry, 0, len(working))
	for _, entry := range working {
		out = append(out, SanitizeEntry(entry, opts))
	}
	return out
}

// SanitizeEntry returns a sanitized copy of a single RequestEntry with auth headers,
// sticky-routing cookies/baggage, sensitive query parameters, and configured JSON body fields scrubbed.
func SanitizeEntry(entry RequestEntry, opts SanitizeOptions) RequestEntry {
	cloned := entry
	cloned.Path = SanitizeURLString(entry.Path, opts.StripAuth, opts.RedactQueryParams)
	if entry.URL != "" {
		cloned.URL = SanitizeURLString(entry.URL, opts.StripAuth, opts.RedactQueryParams)
	}

	if len(entry.Headers) > 0 {
		hdr := make(http.Header, len(entry.Headers))
		for k, v := range entry.Headers {
			hdr.Set(k, v)
		}
		dummyReq := &http.Request{Header: hdr}
		sanitizeRequestHeaders(dummyReq, DefaultHeaderKey, "", opts.StripAuth, opts.StripHeaders)
		if len(dummyReq.Header) == 0 {
			cloned.Headers = nil
		} else {
			cleanHeaders := make(map[string]string, len(dummyReq.Header))
			for k, vals := range dummyReq.Header {
				if len(vals) > 0 {
					cleanHeaders[k] = strings.Join(vals, "; ")
				}
			}
			cloned.Headers = cleanHeaders
		}
	}

	if len(opts.RedactBodyFields) > 0 {
		if cloned.Body != "" {
			if redacted, ok := RedactJSONBodyFields([]byte(cloned.Body), opts.RedactBodyFields); ok {
				cloned.Body = string(redacted)
			}
		} else if cloned.BodyBase64 != "" {
			if raw, err := cloned.PayloadBytes(); err == nil && len(raw) > 0 {
				if redacted, ok := RedactJSONBodyFields(raw, opts.RedactBodyFields); ok {
					cloned.Body = string(redacted)
					cloned.BodyBase64 = ""
				}
			}
		}
	}

	return cloned
}

// SanitizeURLString removes userinfo (when stripAuth is true) and redacts sensitive query parameters
// in rawURL (supporting both absolute URLs and relative paths with query strings).
// If rawURL cannot be parsed by net/url, it fails closed by stripping userinfo and redacting query segments directly.
func SanitizeURLString(rawURL string, stripAuth bool, extraQueryParams []string) string {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return trimmed
	}
	parsed, err := url.Parse(trimmed)
	if err != nil {
		return sanitizeFallbackURLString(trimmed, stripAuth, extraQueryParams)
	}
	sanitizeURLObject(parsed, stripAuth, extraQueryParams)
	out := parsed.String()
	if strings.Contains(trimmed, "{{") {
		out = strings.ReplaceAll(out, "%7B%7B", "{{")
		out = strings.ReplaceAll(out, "%7b%7b", "{{")
		out = strings.ReplaceAll(out, "%7D%7D", "}}")
		out = strings.ReplaceAll(out, "%7d%7d", "}}")
	}
	return out
}

// sanitizeFallbackURLString strips userinfo and redacts sensitive query segments when url.Parse fails on malformed input.
func sanitizeFallbackURLString(trimmed string, stripAuth bool, extraQueryParams []string) string {
	beforeFrag, _, _ := strings.Cut(trimmed, "#")
	beforeQuery, rawQuery, hasQuery := strings.Cut(beforeFrag, "?")
	if stripAuth {
		if scheme, rest, hasScheme := strings.Cut(beforeQuery, "://"); hasScheme {
			slashIdx := strings.IndexByte(rest, '/')
			authority := rest
			pathPart := ""
			if slashIdx >= 0 {
				authority = rest[:slashIdx]
				pathPart = rest[slashIdx:]
			}
			if atIdx := strings.LastIndexByte(authority, '@'); atIdx >= 0 {
				authority = authority[atIdx+1:]
			}
			beforeQuery = scheme + "://" + authority + pathPart
		} else if atIdx := strings.LastIndexByte(beforeQuery, '@'); atIdx >= 0 {
			beforeQuery = beforeQuery[atIdx+1:]
		}
	}
	if !hasQuery || rawQuery == "" {
		return beforeQuery
	}
	redactSet := buildQueryRedactSet(stripAuth, extraQueryParams)
	if len(redactSet) == 0 {
		return beforeQuery + "?" + rawQuery
	}
	redactedQuery, _ := redactRawQuerySegments(rawQuery, redactSet)
	return beforeQuery + "?" + redactedQuery
}

// sanitizeURLObject mutates parsed in place to strip userinfo and redact matching query parameters.
func sanitizeURLObject(parsed *url.URL, stripAuth bool, extraQueryParams []string) {
	if parsed == nil {
		return
	}
	if stripAuth {
		parsed.User = nil
	}
	if parsed.RawQuery == "" {
		return
	}
	redactSet := buildQueryRedactSet(stripAuth, extraQueryParams)
	if len(redactSet) == 0 {
		return
	}
	if redacted, modified := redactRawQuerySegments(parsed.RawQuery, redactSet); modified {
		parsed.RawQuery = redacted
	}
}

// redactRawQuerySegments scans a raw query string split on '&' or ';' without failing open on malformed escapes.
func redactRawQuerySegments(rawQuery string, redactSet map[string]struct{}) (string, bool) {
	if rawQuery == "" || len(redactSet) == 0 {
		return rawQuery, false
	}
	var out strings.Builder
	out.Grow(len(rawQuery))
	modified := false
	start := 0
	for i := 0; i <= len(rawQuery); i++ {
		isEnd := i == len(rawQuery)
		isSep := !isEnd && (rawQuery[i] == '&' || rawQuery[i] == ';')
		if !isEnd && !isSep {
			continue
		}
		seg := rawQuery[start:i]
		if seg != "" {
			keyPart, _, hasEq := strings.Cut(seg, "=")
			unescapedKey, unescapeErr := url.QueryUnescape(keyPart)
			if unescapeErr != nil {
				unescapedKey = keyPart
			}
			lowerKey := strings.ToLower(strings.TrimSpace(unescapedKey))
			_, match := redactSet[lowerKey]
			if match || unescapeErr != nil {
				safeKey := url.QueryEscape(strings.TrimSpace(unescapedKey))
				if unescapeErr != nil {
					safeKey = "redacted_param"
				}
				out.WriteString(safeKey)
				out.WriteByte('=')
				out.WriteString(url.QueryEscape(RedactedPlaceholder))
				modified = true
			} else if hasEq {
				out.WriteString(seg)
			} else {
				out.WriteString(seg)
			}
		}
		if isSep {
			if rawQuery[i] == ';' {
				modified = true
			}
			out.WriteByte('&')
		}
		start = i + 1
	}
	return out.String(), modified
}

// buildQueryRedactSet constructs a lowercase lookup set of query parameter names to redact.
func buildQueryRedactSet(stripAuth bool, extraQueryParams []string) map[string]struct{} {
	if !stripAuth && len(extraQueryParams) == 0 {
		return nil
	}
	out := make(map[string]struct{}, len(defaultAuthQueryParams)+len(extraQueryParams))
	if stripAuth {
		for k := range defaultAuthQueryParams {
			out[k] = struct{}{}
		}
	}
	for _, raw := range extraQueryParams {
		for _, part := range strings.Split(raw, ",") {
			cleaned := strings.ToLower(strings.TrimSpace(part))
			if cleaned != "" {
				out[cleaned] = struct{}{}
			}
		}
	}
	return out
}

// RedactJSONBodyFields redacts any JSON object keys matching redactFields (by leaf key name or dot-separated path)
// in payload, replacing their values with RedactedPlaceholder. Returns the updated JSON bytes and true if payload was valid JSON.
func RedactJSONBodyFields(payload []byte, redactFields []string) ([]byte, bool) {
	trimmed := bytes.TrimSpace(payload)
	if len(trimmed) == 0 || (trimmed[0] != '{' && trimmed[0] != '[') {
		return payload, false
	}

	fieldSet := make(map[string]struct{}, len(redactFields))
	for _, raw := range redactFields {
		for _, part := range strings.Split(raw, ",") {
			cleaned := strings.ToLower(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(part), "$.")))
			if cleaned != "" {
				fieldSet[cleaned] = struct{}{}
			}
		}
	}
	if len(fieldSet) == 0 {
		return payload, false
	}

	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var root any
	if err := dec.Decode(&root); err != nil {
		return payload, false
	}

	redacted, changed := redactJSONValue(root, "", fieldSet)
	if !changed {
		return payload, false
	}

	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(redacted); err != nil {
		return payload, false
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), true
}

// redactJSONValue recursively traverses a decoded JSON value and replaces matching object keys with RedactedPlaceholder.
func redactJSONValue(val any, currentPath string, fieldSet map[string]struct{}) (any, bool) {
	switch v := val.(type) {
	case map[string]any:
		changed := false
		out := make(map[string]any, len(v))
		for k, child := range v {
			lowerKey := strings.ToLower(strings.TrimSpace(k))
			childPath := lowerKey
			if currentPath != "" {
				childPath = currentPath + "." + lowerKey
			}
			_, matchLeaf := fieldSet[lowerKey]
			_, matchPath := fieldSet[childPath]
			if matchLeaf || matchPath {
				out[k] = RedactedPlaceholder
				changed = true
				continue
			}
			newChild, childChanged := redactJSONValue(child, childPath, fieldSet)
			if childChanged {
				changed = true
			}
			out[k] = newChild
		}
		return out, changed
	case []any:
		changed := false
		out := make([]any, len(v))
		for i, elem := range v {
			newElem, elemChanged := redactJSONValue(elem, currentPath, fieldSet)
			if elemChanged {
				changed = true
			}
			out[i] = newElem
		}
		return out, changed
	default:
		return val, false
	}
}

// WriteJSONL serializes entries as newline-delimited JSON (.jsonl) to w.
func WriteJSONL(w io.Writer, entries []RequestEntry) error {
	for i, entry := range entries {
		var buf bytes.Buffer
		enc := json.NewEncoder(&buf)
		enc.SetEscapeHTML(false)
		if err := enc.Encode(entry); err != nil {
			return fmt.Errorf("encode trace entry %d: %w", i, err)
		}
		if _, err := w.Write(buf.Bytes()); err != nil {
			return fmt.Errorf("write trace entry %d: %w", i, err)
		}
	}
	return nil
}

// RecorderProxy is an HTTP reverse proxy that captures and sanitizes live traffic into RequestEntry items.
type RecorderProxy struct {
	TargetURL   *url.URL
	HeaderKey   string
	HeaderValue string
	Sanitize    SanitizeOptions
	AutoChain   bool
	MaxRequests int
	HTTPClient  *http.Client
	OnRecorded  func(RequestEntry)

	mu           sync.Mutex
	out          io.Writer
	entries      []RequestEntry
	counter      int
	autoChainMap map[string]string
}

// NewRecorderServer constructs an http.Server configured for both HTTP/1.1 and unencrypted HTTP/2 (h2c)
// so native gRPC, gRPC-Web, ConnectRPC, and REST clients can record through the reverse proxy.
func NewRecorderServer(handler http.Handler) *http.Server {
	var protocols http.Protocols
	protocols.SetHTTP1(true)
	protocols.SetUnencryptedHTTP2(true)
	return &http.Server{
		Handler:           handler,
		Protocols:         &protocols,
		ReadHeaderTimeout: 10 * time.Second,
	}
}

// NewRecorderProxy constructs a new RecorderProxy forwarding requests to targetURL and writing sanitized JSONL entries to out.
func NewRecorderProxy(targetURL string, out io.Writer, opts SanitizeOptions) (*RecorderProxy, error) {
	if err := loadtest.ValidateTargetURL(targetURL); err != nil {
		return nil, fmt.Errorf("invalid target URL: %w", err)
	}
	parsed, err := url.Parse(strings.TrimSpace(targetURL))
	if err != nil {
		return nil, fmt.Errorf("parse target URL: %w", err)
	}
	client := secureHTTPClient(nil, 30*time.Second)
	client.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &RecorderProxy{
		TargetURL:  parsed,
		Sanitize:   opts,
		HTTPClient: client,
		out:        out,
	}, nil
}

// Entries returns a copy of all recorded RequestEntry items captured so far.
func (p *RecorderProxy) Entries() []RequestEntry {
	p.mu.Lock()
	defer p.mu.Unlock()
	copied := make([]RequestEntry, len(p.entries))
	copy(copied, p.entries)
	return copied
}

// Count returns the number of recorded requests captured so far.
func (p *RecorderProxy) Count() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.entries)
}

// ServeHTTP forwards r to p.TargetURL, streams the upstream response back to w, and records a sanitized RequestEntry.
func (p *RecorderProxy) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	reqBody, err := io.ReadAll(io.LimitReader(r.Body, maxResponseBytes+1))
	if err != nil {
		http.Error(w, "failed to read request body", http.StatusBadRequest)
		return
	}
	if len(reqBody) > maxResponseBytes {
		http.Error(w, "request body exceeds maximum capture size", http.StatusRequestEntityTooLarge)
		return
	}

	upstreamURL := *p.TargetURL
	basePrefix := strings.TrimRight(p.TargetURL.Path, "/")
	reqPath := r.URL.Path
	if !strings.HasPrefix(reqPath, "/") {
		reqPath = "/" + reqPath
	}
	upstreamURL.Path = basePrefix + reqPath
	upstreamURL.RawQuery = r.URL.RawQuery

	outReq, err := http.NewRequestWithContext(r.Context(), r.Method, upstreamURL.String(), bytes.NewReader(reqBody))
	if err != nil {
		http.Error(w, "failed to build upstream request", http.StatusInternalServerError)
		return
	}
	for k, vals := range r.Header {
		lower := strings.ToLower(k)
		if lower == "host" || lower == "connection" || lower == "content-length" {
			continue
		}
		for _, v := range vals {
			outReq.Header.Add(k, v)
		}
	}
	if hk := strings.TrimSpace(p.HeaderKey); hk != "" && p.HeaderValue != "" && outReq.Header.Get(hk) == "" {
		outReq.Header.Set(hk, p.HeaderValue)
	}

	baseClient := p.HTTPClient
	if baseClient == nil {
		baseClient = secureHTTPClient(nil, 30*time.Second)
	}
	noRedirectClient := *baseClient
	noRedirectClient.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	resp, err := noRedirectClient.Do(outReq)
	if err != nil {
		http.Error(w, fmt.Sprintf("upstream request failed: %v", err), http.StatusBadGateway)
		return
	}
	defer func() { _ = resp.Body.Close() }()

	respBody, _ := io.ReadAll(io.LimitReader(resp.Body, maxResponseBytes))
	for k, vals := range resp.Header {
		for _, v := range vals {
			w.Header().Add(k, v)
		}
	}
	for k := range resp.Trailer {
		w.Header().Add("Trailer", k)
	}
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(respBody)
	for k, vals := range resp.Trailer {
		for _, v := range vals {
			w.Header().Add(http.TrailerPrefix+k, v)
		}
	}

	recordedPath := reqPath
	if r.URL.RawQuery != "" {
		recordedPath = reqPath + "?" + r.URL.RawQuery
	}

	headersMap := make(map[string]string)
	for k, vals := range r.Header {
		lower := strings.ToLower(k)
		if lower == "host" || lower == "connection" || lower == "content-length" || lower == "accept-encoding" {
			continue
		}
		if len(vals) > 0 {
			headersMap[k] = strings.Join(vals, "; ")
		}
	}
	if len(headersMap) == 0 {
		headersMap = nil
	}

	p.mu.Lock()
	if p.MaxRequests > 0 && len(p.entries) >= p.MaxRequests {
		p.mu.Unlock()
		return
	}
	p.counter++
	if p.AutoChain && len(p.autoChainMap) > 0 {
		recordedPath = rewriteAutoChainedPath(recordedPath, p.autoChainMap)
	}
	entry := RequestEntry{
		ID:           fmt.Sprintf("rec-%d", p.counter),
		Method:       r.Method,
		Path:         recordedPath,
		Headers:      headersMap,
		ExpectStatus: resp.StatusCode,
	}
	if len(reqBody) > 0 {
		ct := strings.ToLower(r.Header.Get("Content-Type"))
		if isBinaryContentType(ct) {
			entry.BodyBase64 = encodeBase64(reqBody)
		} else {
			entry.Body = string(reqBody)
		}
	}
	if p.AutoChain && (r.Method == http.MethodPost || r.Method == http.MethodPut) && resp.StatusCode >= 200 && resp.StatusCode < 300 {
		if varName, selector, rawVal, ok := detectAutoChainID(p.counter, respBody); ok {
			if p.autoChainMap == nil {
				p.autoChainMap = make(map[string]string)
			}
			p.autoChainMap[rawVal] = "{{" + varName + "}}"
			entry.Extract = map[string]string{varName: selector}
		}
	}
	sanOpts := p.Sanitize
	if hk := strings.TrimSpace(p.HeaderKey); hk != "" {
		sanOpts.StripHeaders = append(append([]string(nil), p.Sanitize.StripHeaders...), hk)
	}
	sanitized := SanitizeEntry(entry, sanOpts)
	p.entries = append(p.entries, sanitized)
	if p.out != nil {
		_ = WriteJSONL(p.out, []RequestEntry{sanitized})
	}
	onRec := p.OnRecorded
	p.mu.Unlock()

	if onRec != nil {
		onRec(sanitized)
	}
}

// isBinaryContentType reports whether contentType represents a binary Protobuf or gRPC payload.
func isBinaryContentType(contentType string) bool {
	return strings.Contains(contentType, "proto") ||
		strings.Contains(contentType, "grpc") ||
		strings.Contains(contentType, "octet-stream")
}

// encodeBase64 encodes raw bytes as standard base64.
func encodeBase64(b []byte) string {
	return base64.StdEncoding.EncodeToString(b)
}

// detectAutoChainID inspects a 2xx JSON response body for a top-level "id" or "uuid" field and returns (varName, selector, rawValue, true).
func detectAutoChainID(stepNum int, respBody []byte) (string, string, string, bool) {
	trimmed := bytes.TrimSpace(respBody)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return "", "", "", false
	}
	dec := json.NewDecoder(bytes.NewReader(trimmed))
	dec.UseNumber()
	var obj map[string]any
	if err := dec.Decode(&obj); err != nil {
		return "", "", "", false
	}
	for _, key := range []string{"id", "uuid"} {
		val, exists := obj[key]
		if !exists || val == nil {
			continue
		}
		var strVal string
		switch v := val.(type) {
		case string:
			strVal = strings.TrimSpace(v)
		case json.Number:
			strVal = v.String()
		}
		if strVal != "" {
			varName := fmt.Sprintf("step%d_%s", stepNum, key)
			return varName, "$." + key, strVal, true
		}
	}
	return "", "", "", false
}

// rewriteAutoChainedPath replaces path segments matching previously captured resource IDs with {{varName}} placeholders.
func rewriteAutoChainedPath(rawPath string, chainMap map[string]string) string {
	if len(chainMap) == 0 || rawPath == "" {
		return rawPath
	}
	pathPart, queryPart, hasQuery := strings.Cut(rawPath, "?")
	segs := strings.Split(pathPart, "/")
	changed := false
	for i, seg := range segs {
		if placeholder, ok := chainMap[seg]; ok && seg != "" {
			segs[i] = placeholder
			changed = true
		}
	}
	if !changed {
		return rawPath
	}
	rebuilt := strings.Join(segs, "/")
	if hasQuery {
		return rebuilt + "?" + queryPart
	}
	return rebuilt
}

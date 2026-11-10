package replay

import (
	"bytes"
	"context"
	"encoding/base64"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSanitizeURLString(t *testing.T) {
	raw := "https://alice:s3cret@api.example.com/v1/items?token=jwt-123&api_key=ak-999&limit=20&session_id=sid-42"

	// With stripAuth=true and custom RedactQueryParams
	got := SanitizeURLString(raw, true, []string{"session_id"})
	assert.NotContains(t, got, "alice:s3cret")
	assert.NotContains(t, got, "jwt-123")
	assert.NotContains(t, got, "ak-999")
	assert.NotContains(t, got, "sid-42")
	assert.Contains(t, got, "limit=20")
	assert.Contains(t, got, "token=%5BREDACTED%5D")
	assert.Contains(t, got, "api_key=%5BREDACTED%5D")
	assert.Contains(t, got, "session_id=%5BREDACTED%5D")

	// With stripAuth=false and only custom RedactQueryParams
	gotCustomOnly := SanitizeURLString("/v1/items?token=keep-me&ssn=123-45-6789", false, []string{"ssn"})
	assert.Contains(t, gotCustomOnly, "token=keep-me")
	assert.NotContains(t, gotCustomOnly, "123-45-6789")
	assert.Contains(t, gotCustomOnly, "ssn=%5BREDACTED%5D")

	// Semicolon query separators and malformed percent escapes fail closed
	gotSemicolon := SanitizeURLString("/v1/items?token=semi-secret;keep=1&api_key=bad%ZZescape", true, nil)
	assert.NotContains(t, gotSemicolon, "semi-secret")
	assert.NotContains(t, gotSemicolon, "bad%ZZescape")
	assert.Contains(t, gotSemicolon, "keep=1")
	assert.Contains(t, gotSemicolon, "token=%5BREDACTED%5D")
	assert.Contains(t, gotSemicolon, "api_key=%5BREDACTED%5D")

	// Fallback when url.Parse itself fails on control char or malformed URL
	gotMalformedURL := SanitizeURLString("https://bob:topsecret@api.example.com/\x00path?password=leaked&ok=true", true, nil)
	assert.NotContains(t, gotMalformedURL, "bob:topsecret")
	assert.NotContains(t, gotMalformedURL, "leaked")
	assert.Contains(t, gotMalformedURL, "password=%5BREDACTED%5D")
	assert.Contains(t, gotMalformedURL, "ok=true")
}

func TestRedactJSONBodyFields(t *testing.T) {
	payload := []byte(`{"user":{"email":"alice@example.com","password":"p@ssw0rd","ssn":"000-11-2222"},"items":[{"card_number":"4111222233334444","qty":2}]}`)

	redacted, ok := RedactJSONBodyFields(payload, []string{"password", "$.user.ssn", "card_number"})
	require.True(t, ok)
	s := string(redacted)
	assert.Contains(t, s, `"email":"alice@example.com"`)
	assert.Contains(t, s, `"password":"[REDACTED]"`)
	assert.Contains(t, s, `"ssn":"[REDACTED]"`)
	assert.Contains(t, s, `"card_number":"[REDACTED]"`)
	assert.Contains(t, s, `"qty":2`)
	assert.NotContains(t, s, "p@ssw0rd")
	assert.NotContains(t, s, "000-11-2222")
	assert.NotContains(t, s, "4111222233334444")

	// Non-JSON payload is returned unchanged with ok=false
	nonJSON := []byte("plain text body")
	unchanged, ok := RedactJSONBodyFields(nonJSON, []string{"password"})
	assert.False(t, ok)
	assert.Equal(t, nonJSON, unchanged)
}

func TestSanitizeEntriesAndWriteJSONL(t *testing.T) {
	b64JSON := base64.StdEncoding.EncodeToString([]byte(`{"secret":"top-secret","public":"ok"}`))
	entries := []RequestEntry{
		{
			ID:     "req-1",
			Method: "POST",
			Path:   "https://api.example.com/v1/login?access_token=tok-1&page=1",
			Headers: map[string]string{
				"Authorization": "Bearer secret-jwt",
				"Cookie":        "diverge_env=pr-99; session=s1",
				"X-Custom-Key":  "custom-secret",
				"Content-Type":  "application/json",
			},
			Body: `{"password":"my-password","username":"alice"}`,
		},
		{
			ID:         "req-2",
			Method:     "POST",
			Path:       "/v1/encoded",
			BodyBase64: b64JSON,
		},
		{
			ID:     "req-drop",
			Method: "GET",
			Path:   "https://analytics.other.io/ping",
		},
	}

	sanitized := SanitizeEntries(entries, SanitizeOptions{
		StripAuth:         true,
		StripHeaders:      []string{"X-Custom-Key"},
		RedactQueryParams: []string{"extra_q"},
		RedactBodyFields:  []string{"password", "secret"},
		FilterHosts:       []string{"api.example.com"},
	})
	require.Len(t, sanitized, 2)

	assert.NotContains(t, sanitized[0].Path, "tok-1")
	assert.Contains(t, sanitized[0].Path, "page=1")
	assert.NotContains(t, sanitized[0].Headers, "Authorization")
	assert.NotContains(t, sanitized[0].Headers, "Cookie")
	assert.NotContains(t, sanitized[0].Headers, "X-Custom-Key")
	assert.Equal(t, "application/json", sanitized[0].Headers["Content-Type"])
	assert.Contains(t, sanitized[0].Body, `"password":"[REDACTED]"`)
	assert.Contains(t, sanitized[0].Body, `"username":"alice"`)

	// Base64-encoded JSON body is decoded, redacted, and stored as clean JSON Body
	assert.Empty(t, sanitized[1].BodyBase64)
	assert.Contains(t, sanitized[1].Body, `"secret":"[REDACTED]"`)
	assert.Contains(t, sanitized[1].Body, `"public":"ok"`)

	var jsonlBuf bytes.Buffer
	require.NoError(t, WriteJSONL(&jsonlBuf, sanitized))
	reloaded, err := ParseTrace(jsonlBuf.Bytes(), ".jsonl")
	require.NoError(t, err)
	require.Len(t, reloaded, 2)
	assert.Equal(t, sanitized[0].ID, reloaded[0].ID)
	assert.Equal(t, sanitized[1].Body, reloaded[1].Body)
}

func TestRecorderProxy_CapturesAndSanitizesLiveTraffic(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/api/v1/checkout":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"orderId":"ord-100"}`))
		case "/acme.v1.OrderService/Get":
			w.Header().Set("Content-Type", "application/proto")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte{0x08, 0x01})
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	// Rejects SSRF target URL
	_, err := NewRecorderProxy("http://169.254.169.254", io.Discard, SanitizeOptions{})
	require.Error(t, err)

	var jsonlBuf bytes.Buffer
	proxy, err := NewRecorderProxy(upstream.URL, &jsonlBuf, SanitizeOptions{
		StripAuth:         true,
		RedactQueryParams: []string{"api_secret"},
		RedactBodyFields:  []string{"cvv"},
	})
	require.NoError(t, err)
	proxy.HTTPClient = upstream.Client()
	proxy.MaxRequests = 2

	var recordedIDs []string
	var recMu sync.Mutex
	proxy.OnRecorded = func(entry RequestEntry) {
		recMu.Lock()
		recordedIDs = append(recordedIDs, entry.ID)
		recMu.Unlock()
	}

	proxySrv := httptest.NewServer(proxy)
	defer proxySrv.Close()

	// 1. JSON POST with auth header, query secret, and body CVV
	req1, err := http.NewRequest("POST", proxySrv.URL+"/api/v1/checkout?token=jwt-abc&api_secret=sec-1&org=acme", strings.NewReader(`{"amount":500,"cvv":"999"}`))
	require.NoError(t, err)
	req1.Header.Set("Authorization", "Bearer my-jwt")
	req1.Header.Set("Content-Type", "application/json")
	resp1, err := proxySrv.Client().Do(req1)
	require.NoError(t, err)
	_ = resp1.Body.Close()
	assert.Equal(t, http.StatusCreated, resp1.StatusCode)

	// 2. Binary Protobuf POST
	req2, err := http.NewRequest("POST", proxySrv.URL+"/acme.v1.OrderService/Get", bytes.NewReader([]byte{0x08, 0x2a}))
	require.NoError(t, err)
	req2.Header.Set("Content-Type", "application/proto")
	resp2, err := proxySrv.Client().Do(req2)
	require.NoError(t, err)
	_ = resp2.Body.Close()
	assert.Equal(t, http.StatusOK, resp2.StatusCode)

	// 3. Third request exceeds MaxRequests=2 (still proxied, but not recorded)
	req3, err := http.NewRequest("GET", proxySrv.URL+"/api/v1/checkout", nil)
	require.NoError(t, err)
	resp3, err := proxySrv.Client().Do(req3)
	require.NoError(t, err)
	_ = resp3.Body.Close()

	assert.Equal(t, 2, proxy.Count())
	entries := proxy.Entries()
	require.Len(t, entries, 2)
	assert.Equal(t, []string{"rec-1", "rec-2"}, recordedIDs)

	assert.Equal(t, http.StatusCreated, entries[0].ExpectStatus)
	assert.NotContains(t, entries[0].Path, "jwt-abc")
	assert.NotContains(t, entries[0].Path, "sec-1")
	assert.Contains(t, entries[0].Path, "org=acme")
	assert.NotContains(t, entries[0].Headers, "Authorization")
	assert.Contains(t, entries[0].Body, `"cvv":"[REDACTED]"`)
	assert.NotContains(t, entries[0].Body, "999")

	assert.Equal(t, base64.StdEncoding.EncodeToString([]byte{0x08, 0x2a}), entries[1].BodyBase64)
}

func TestRunner_RedactsQueryParamsAndBodyFieldsOnReplay(t *testing.T) {
	var (
		mu          sync.Mutex
		seenQueries []string
		seenBodies  []string
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		bodyBytes, _ := io.ReadAll(r.Body)
		mu.Lock()
		seenQueries = append(seenQueries, r.URL.RawQuery)
		seenBodies = append(seenBodies, string(bodyBytes))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	entries := []RequestEntry{
		{
			ID:     "redact-live",
			Method: "POST",
			Path:   "/api/v1/auth?token=live-jwt&custom_secret=my-sec&keep=1",
			Headers: map[string]string{
				"Content-Type": "application/json",
			},
			Body: `{"password":"super-secret-password","username":"bob"}`,
		},
	}

	runner := NewRunner(Config{
		Environment:        "pr-42",
		BaseURL:            srv.URL,
		HeaderValue:        "pr-42",
		StripAuth:          true,
		RedactQueryParams:  []string{"custom_secret"},
		RedactBodyFields:   []string{"password"},
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		HTTPClient:         srv.Client(),
	})

	report, err := runner.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.True(t, report.Passed)
	require.Len(t, report.Results, 1)
	assert.NotContains(t, report.Results[0].Path, "live-jwt")
	assert.NotContains(t, report.Results[0].Path, "my-sec")

	mu.Lock()
	defer mu.Unlock()
	require.Len(t, seenQueries, 2)
	for _, q := range seenQueries {
		assert.NotContains(t, q, "live-jwt")
		assert.NotContains(t, q, "my-sec")
		assert.Contains(t, q, "keep=1")
	}
	for _, b := range seenBodies {
		assert.NotContains(t, b, "super-secret-password")
		assert.Contains(t, b, `"password":"[REDACTED]"`)
		assert.Contains(t, b, `"username":"bob"`)
	}
}

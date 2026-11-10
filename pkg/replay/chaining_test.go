package replay

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestInterpolateAndExtractResponseVars(t *testing.T) {
	vars := map[string]string{
		"order_id": "ord-999",
		"token":    "tok-abc",
	}
	assert.Equal(t, "/api/v1/orders/ord-999/items?keep={{unknown}}", InterpolateString("/api/v1/orders/{{order_id}}/items?keep={{unknown}}", vars))

	hdrs := InterpolateHeaders(map[string]string{"X-Order": "{{ order_id }}", "X-Other": "static"}, vars)
	assert.Equal(t, "ord-999", hdrs["X-Order"])
	assert.Equal(t, "static", hdrs["X-Other"])

	body := InterpolateBytes([]byte(`{"orderId":"{{order_id}}","token":"{{token}}"}`), vars)
	assert.Equal(t, `{"orderId":"ord-999","token":"tok-abc"}`, string(body))

	respHeaders := make(http.Header)
	respHeaders.Set("X-Session-Id", "sess-42")
	respBody := []byte(`{"id":"ord-777","nested":{"amount":1999,"items":[{"sku":"sku-1"},{"sku":"sku-2"}]},"active":true}`)

	extracted, err := ExtractResponseVars(map[string]string{
		"order_id":   "$.id",
		"amount":     "$.nested.amount",
		"second_sku": "$.nested.items[1].sku",
		"active":     "$.active",
		"session_id": "$header.X-Session-Id",
		"http_code":  "$status",
	}, http.StatusCreated, respHeaders, respBody)
	require.NoError(t, err)
	assert.Equal(t, "ord-777", extracted["order_id"])
	assert.Equal(t, "1999", extracted["amount"])
	assert.Equal(t, "sku-2", extracted["second_sku"])
	assert.Equal(t, "true", extracted["active"])
	assert.Equal(t, "sess-42", extracted["session_id"])
	assert.Equal(t, "201", extracted["http_code"])
}

func TestRunner_StatefulMultiStepChainingWithDivergentIDs(t *testing.T) {
	// Baseline generates "ord-base-100" while Preview generates "ord-prev-200".
	// Step 2 (GET /api/v1/orders/{{order_id}}) and Step 3 (POST /api/v1/orders/{{order_id}}/confirm)
	// must route each environment's own extracted order_id and session header.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-42"
		w.Header().Set("Content-Type", "application/json")

		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/orders":
			if isPreview {
				w.Header().Set("X-Order-Token", "prev-tok-200")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"ord-prev-200","status":"created"}`))
			} else {
				w.Header().Set("X-Order-Token", "base-tok-100")
				w.WriteHeader(http.StatusCreated)
				_, _ = w.Write([]byte(`{"id":"ord-base-100","status":"created"}`))
			}
		case r.Method == http.MethodGet && strings.HasPrefix(r.URL.Path, "/api/v1/orders/"):
			expectedID := "ord-base-100"
			expectedTok := "base-tok-100"
			if isPreview {
				expectedID = "ord-prev-200"
				expectedTok = "prev-tok-200"
			}
			if r.URL.Path != "/api/v1/orders/"+expectedID || r.Header.Get("X-Order-Token") != expectedTok {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"error":"wrong order id or token"}`))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"` + expectedID + `","status":"ready","items":[{"sku":"item-1"}]}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	traceJSONL := strings.Join([]string{
		`{"id":"create-order","method":"POST","path":"/api/v1/orders","body":"{\"sku\":\"item-1\"}","expect_status":201,"extract":{"order_id":"$.id","order_tok":"$header.X-Order-Token"}}`,
		`{"id":"get-order","method":"GET","path":"/api/v1/orders/{{order_id}}","headers":{"X-Order-Token":"{{order_tok}}"},"expect_status":200}`,
	}, "\n")

	entries, err := ParseTrace([]byte(traceJSONL), ".jsonl")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.True(t, HasChaining(entries))

	runner := NewRunner(Config{
		Environment:        "pr-42",
		BaseURL:            srv.URL,
		HeaderKey:          "x-diverge-env",
		HeaderValue:        "pr-42",
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		HTTPClient:         srv.Client(),
	})

	report, err := runner.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.True(t, report.Passed)
	require.Len(t, report.Results, 2)
	assert.Equal(t, "ord-prev-200", report.Results[0].ExtractedVars["order_id"])
	assert.Equal(t, "prev-tok-200", report.Results[0].ExtractedVars["order_tok"])
	assert.Equal(t, "/api/v1/orders/ord-prev-200", report.Results[1].Path)
}

func TestRecorderProxy_AutoChainAndH2CTrailers(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPost && r.URL.Path == "/api/v1/carts":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id":"cart-abc-999","items":0}`))
		case r.Method == http.MethodGet && r.URL.Path == "/api/v1/carts/cart-abc-999":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"cart-abc-999","items":1}`))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer upstream.Close()

	var buf bytes.Buffer
	proxy, err := NewRecorderProxy(upstream.URL, &buf, SanitizeOptions{StripAuth: true})
	require.NoError(t, err)
	proxy.AutoChain = true
	proxy.HTTPClient = upstream.Client()

	recSrv := httptest.NewServer(proxy)
	defer recSrv.Close()

	resp1, err := recSrv.Client().Post(recSrv.URL+"/api/v1/carts", "application/json", strings.NewReader(`{}`))
	require.NoError(t, err)
	_ = resp1.Body.Close()

	resp2, err := recSrv.Client().Get(recSrv.URL + "/api/v1/carts/cart-abc-999?view=full")
	require.NoError(t, err)
	_ = resp2.Body.Close()

	entries := proxy.Entries()
	require.Len(t, entries, 2)
	assert.Equal(t, map[string]string{"step1_id": "$.id"}, entries[0].Extract)
	assert.Equal(t, "/api/v1/carts/{{step1_id}}?view=full", entries[1].Path)
}

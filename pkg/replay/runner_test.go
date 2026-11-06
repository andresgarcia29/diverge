package replay

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
)

func TestRunner_ReplayJSONAndProtobuf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-42"

		switch r.URL.Path {
		case "/api/v1/orders":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if isPreview {
				// Preview adds a non-breaking field
				_, _ = w.Write([]byte(`{"id":"ord-1","total":4200,"currency":"USD","preview_tag":"v2"}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"ord-1","total":4200,"currency":"USD"}`))
			}

		case "/acme.v1.PaymentService/Charge":
			w.Header().Set("Content-Type", "application/proto")
			var msg []byte
			msg = protowire.AppendTag(msg, 1, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 1)
			if !isPreview {
				// Baseline has field 2 (receipt_url), preview drops it!
				msg = protowire.AppendTag(msg, 2, protowire.BytesType)
				msg = protowire.AppendString(msg, "https://receipts.example.com/1")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(msg)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	entries := []RequestEntry{
		{
			ID:     "list-orders",
			Method: "GET",
			Path:   "/api/v1/orders",
		},
		{
			ID:      "charge-rpc",
			Method:  "POST",
			Path:    "/acme.v1.PaymentService/Charge",
			Headers: map[string]string{"Content-Type": "application/proto"},
		},
	}

	runner := NewRunner(Config{
		Environment:        "pr-42",
		Namespace:          "default",
		BaseURL:            srv.URL,
		HeaderKey:          "x-diverge-env",
		HeaderValue:        "pr-42",
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		HTTPClient:         srv.Client(),
	})

	report, err := runner.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.False(t, report.Passed)
	assert.Equal(t, 2, report.TotalRequests)
	assert.Equal(t, 1, report.PassedRequests)
	assert.Equal(t, 1, report.FailedRequests)
	assert.Equal(t, 1, report.BreakingDiffs)

	// Entry 0 (JSON with non-breaking extra field) passes
	assert.True(t, report.Results[0].Passed)
	assert.Equal(t, ProtocolJSON, report.Results[0].Protocol)
	require.Len(t, report.Results[0].ContractDiffs, 1)
	assert.False(t, report.Results[0].ContractDiffs[0].Breaking)

	// Entry 1 (Protobuf missing field 2) fails with breaking diff
	assert.False(t, report.Results[1].Passed)
	assert.Equal(t, ProtocolProtobuf, report.Results[1].Protocol)
	require.Len(t, report.Results[1].ContractDiffs, 1)
	assert.Equal(t, "proto.field_2", report.Results[1].ContractDiffs[0].Path)
	assert.Equal(t, "missing_field", report.Results[1].ContractDiffs[0].Kind)
	assert.True(t, report.Results[1].ContractDiffs[0].Breaking)

	var tableBuf, mdBuf, jsonBuf bytes.Buffer
	FormatTable(&tableBuf, report)
	assert.Contains(t, tableBuf.String(), "charge-rpc")
	assert.Contains(t, tableBuf.String(), "[FAIL]")

	FormatMarkdown(&mdBuf, report)
	assert.Contains(t, mdBuf.String(), "Diverge Traffic Replay Failed: pr-42")
	assert.Contains(t, mdBuf.String(), "`proto.field_2`")

	require.NoError(t, FormatJSON(&jsonBuf, report))
	assert.Contains(t, jsonBuf.String(), `"breakingDiffs": 1`)
}

func TestResolveEntryURL_RewriteAndSSRF(t *testing.T) {
	// Rewrites full HAR URL host when BaseURL is provided
	rewritten, err := ResolveEntryURL("https://staging.example.com", "https://prod.example.com/api/v1/items?limit=10")
	require.NoError(t, err)
	assert.Equal(t, "https://staging.example.com/api/v1/items?limit=10", rewritten)

	// Blocks cloud metadata SSRF
	_, err = ResolveEntryURL("http://169.254.169.254", "/latest/meta-data")
	assert.Error(t, err)
}

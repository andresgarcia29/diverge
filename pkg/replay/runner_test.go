package replay

import (
	"bytes"
	"context"
	"encoding/binary"
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

	require.NoError(t, FormatMarkdown(&mdBuf, report))
	assert.Contains(t, mdBuf.String(), "Diverge Traffic Replay Failed: pr-42")
	assert.Contains(t, mdBuf.String(), "`proto.field_2`")

	require.NoError(t, FormatJSON(&jsonBuf, report))
	assert.Contains(t, jsonBuf.String(), `"breakingDiffs": 1`)
}

func TestRunner_StripsRecordedPreviewHeadersFromBaselineAndChecksGRPCStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-42" || r.Header.Get("baggage") == "diverge.env=pr-42"

		switch r.URL.Path {
		case "/api/v1/recorded":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if isPreview {
				_, _ = w.Write([]byte(`{"env":"preview"}`))
			} else {
				_, _ = w.Write([]byte(`{"env":"baseline"}`))
			}

		case "/acme.v1.Health/Check":
			w.Header().Set("Content-Type", "application/grpc+proto")
			var msg []byte
			msg = protowire.AppendTag(msg, 1, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 1)
			framed := make([]byte, 5+len(msg))
			binary.BigEndian.PutUint32(framed[1:5], uint32(len(msg)))
			copy(framed[5:], msg)
			if isPreview {
				w.Header().Set("Grpc-Status", "14") // UNAVAILABLE
			} else {
				w.Header().Set("Grpc-Status", "0")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(framed)

		case "/redirect-ssrf":
			http.Redirect(w, r, "http://169.254.169.254/latest/meta-data", http.StatusFound)
		}
	}))
	defer srv.Close()

	entries := []RequestEntry{
		{
			ID:     "recorded-har-header",
			Method: "GET",
			Path:   "/api/v1/recorded",
			Headers: map[string]string{
				"x-diverge-env": "pr-42",
				"baggage":       "diverge.env=pr-42,user=alice",
			},
		},
		{
			ID:     "grpc-status-diff",
			Method: "POST",
			Path:   "/acme.v1.Health/Check",
		},
		{
			ID:     "redirect-ssrf",
			Method: "GET",
			Path:   "/redirect-ssrf",
		},
	}

	runner := NewRunner(Config{
		Environment:        "pr-42",
		BaseURL:            srv.URL,
		HeaderKey:          "x-diverge-env",
		HeaderValue:        "pr-42",
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		DiffOptions: DiffOptions{
			CompareValues: true,
		},
	})

	report, err := runner.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.False(t, report.Passed)
	require.Len(t, report.Results, 3)

	// Entry 0: baseline had x-diverge-env stripped so it received {"env":"baseline"} vs preview {"env":"preview"}
	assert.False(t, report.Results[0].Passed)
	require.Len(t, report.Results[0].ContractDiffs, 1)
	assert.Equal(t, `"baseline"`, report.Results[0].ContractDiffs[0].Baseline)
	assert.Equal(t, `"preview"`, report.Results[0].ContractDiffs[0].Preview)

	// Entry 1: identical protobuf messages, but preview returned Grpc-Status: 14
	assert.False(t, report.Results[1].Passed)
	assert.False(t, report.Results[1].StatusMatch)
	require.Len(t, report.Results[1].ContractDiffs, 1)
	assert.Equal(t, "grpc.status", report.Results[1].ContractDiffs[0].Path)
	assert.Contains(t, report.Results[1].FailureReasons[0], "gRPC status mismatch")

	// Entry 2: redirect to metadata IP blocked by CheckRedirect
	assert.False(t, report.Results[2].Passed)
	assert.Contains(t, report.Results[2].FailureReasons[0], "redirect target prohibited")
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

func TestFilterEntriesByHostAndStripAuth(t *testing.T) {
	var seenAuth, seenCookie, seenCustom string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		seenAuth = r.Header.Get("Authorization")
		seenCookie = r.Header.Get("Cookie")
		seenCustom = r.Header.Get("X-Secret-Token")
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	entries := []RequestEntry{
		{
			ID:     "keep-staging",
			Method: "GET",
			Path:   "https://staging.example.com/api/v1/orders",
			Headers: map[string]string{
				"Authorization":  "Bearer secret-jwt",
				"Cookie":         "diverge_env=pr-10; session=abc",
				"X-Secret-Token": "top-secret",
			},
		},
		{
			ID:     "drop-analytics",
			Method: "POST",
			Path:   "https://analytics.thirdparty.io/collect",
		},
	}

	// Verify cookie stripping when StripAuth is false
	runnerKeepSession := NewRunner(Config{
		Environment:        "pr-42",
		BaseURL:            srv.URL,
		FilterHosts:        []string{"*.example.com"},
		StripHeaders:       []string{"X-Secret-Token"},
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
	})
	report, err := runnerKeepSession.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.True(t, report.Passed)
	require.Len(t, report.Results, 1)
	assert.Equal(t, "keep-staging", report.Results[0].ID)
	assert.Equal(t, "Bearer secret-jwt", seenAuth)
	assert.Equal(t, "session=abc", seenCookie)
	assert.Empty(t, seenCustom)

	// Verify StripAuth removes Authorization and Cookie completely
	runnerStripAuth := NewRunner(Config{
		Environment:        "pr-42",
		BaseURL:            srv.URL,
		FilterHosts:        []string{"staging.example.com"},
		StripAuth:          true,
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
	})
	report2, err := runnerStripAuth.Run(context.Background(), entries)
	require.NoError(t, err)
	assert.True(t, report2.Passed)
	assert.Empty(t, seenAuth)
	assert.Empty(t, seenCookie)
}

func TestFormatReportOutputs(t *testing.T) {
	report := &Report{
		Environment:      "pr-42",
		TargetURL:        "https://staging.example.com",
		RoutingHeader:    "x-diverge-env: pr-42",
		Passed:           false,
		TotalRequests:    2,
		PassedRequests:   1,
		FailedRequests:   1,
		StatusMismatches: 0,
		BreakingDiffs:    1,
		BaselineP50Ms:    12,
		PreviewP50Ms:     15,
		BaselineP95Ms:    25,
		PreviewP95Ms:     30,
		Results: []EntryResult{
			{
				ID:                "req-1",
				Method:            "GET",
				Path:              "/api/v1/items",
				Protocol:          ProtocolJSON,
				BaselineStatus:    200,
				PreviewStatus:     200,
				BaselineLatencyMs: 12,
				PreviewLatencyMs:  15,
				Passed:            true,
				StatusMatch:       true,
				ContractDiffs: []ContractDiff{
					{
						Path:     "$.extra",
						Kind:     "extra_field",
						Preview:  "string",
						Breaking: false,
					},
				},
			},
			{
				ID:                "req-2",
				Method:            "POST",
				Path:              "/acme.v1.Order|Create",
				Protocol:          ProtocolProtobuf,
				BaselineStatus:    200,
				PreviewStatus:     200,
				BaselineLatencyMs: 25,
				PreviewLatencyMs:  30,
				Passed:            false,
				StatusMatch:       true,
				BreakingDiffCount: 1,
				FailureReasons:    []string{"1 breaking contract diff(s) (e.g. proto.field_2: missing_field)"},
				ContractDiffs: []ContractDiff{
					{
						Path:     "proto.field_2|sub",
						Kind:     "missing_field",
						Baseline: "bytes|string",
						Breaking: true,
					},
				},
			},
		},
	}

	var tableBuf bytes.Buffer
	FormatTable(&tableBuf, report)
	tableStr := tableBuf.String()
	assert.Contains(t, tableStr, "[PASS]")
	assert.Contains(t, tableStr, "[FAIL]")
	assert.Contains(t, tableStr, "1 non-breaking diff(s)")
	assert.Contains(t, tableStr, "Replay FAILED")

	var mdBuf bytes.Buffer
	require.NoError(t, FormatMarkdown(&mdBuf, report))
	mdStr := mdBuf.String()
	assert.Contains(t, mdStr, "### ❌ Diverge Traffic Replay Failed: pr-42")
	assert.Contains(t, mdStr, "#### 🔍 Contract & Wire Diffs")
	assert.Contains(t, mdStr, "| req-2 (`/acme.v1.Order\\|Create`) | `proto.field_2\\|sub` | `missing_field` | `bytes\\|string` | `` | **Yes** |")

	var jsonBuf bytes.Buffer
	require.NoError(t, FormatJSON(&jsonBuf, report))
	assert.Contains(t, jsonBuf.String(), `"environment": "pr-42"`)
}

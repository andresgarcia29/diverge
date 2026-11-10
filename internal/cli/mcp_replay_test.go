package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/protocgen/proto2mcp/pkg/mcpruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/divergedev/diverge/pkg/replay"
)

func TestMCPReplay_InlineJSONAndProtobuf(t *testing.T) {
	type capturedInvoiceReq struct {
		method      string
		contentType string
		body        []byte
	}
	var (
		mu              sync.Mutex
		baseInvoiceReq  capturedInvoiceReq
		prevInvoiceReq  capturedInvoiceReq
		baseReqCaptured bool
		prevReqCaptured bool
		baseProfileAuth string
		prevProfileAuth string
	)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-99"
		switch r.URL.Path {
		case "/api/v1/profile":
			mu.Lock()
			if isPreview {
				prevProfileAuth = r.Header.Get("Authorization")
			} else {
				baseProfileAuth = r.Header.Get("Authorization")
			}
			mu.Unlock()
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"u-1","plan":"pro"}`))

		case "/acme.v1.Billing/GetInvoice":
			reqBytes, _ := io.ReadAll(r.Body)
			mu.Lock()
			if isPreview {
				prevInvoiceReq = capturedInvoiceReq{
					method:      r.Method,
					contentType: r.Header.Get("Content-Type"),
					body:        reqBytes,
				}
				prevReqCaptured = true
			} else {
				baseInvoiceReq = capturedInvoiceReq{
					method:      r.Method,
					contentType: r.Header.Get("Content-Type"),
					body:        reqBytes,
				}
				baseReqCaptured = true
			}
			mu.Unlock()

			w.Header().Set("Content-Type", "application/proto")
			var msg []byte
			msg = protowire.AppendTag(msg, 1, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 5000)
			if !isPreview {
				msg = protowire.AppendTag(msg, 2, protowire.BytesType)
				msg = protowire.AppendString(msg, "USD")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(msg)
		}
	}))
	defer srv.Close()

	registry := mcpruntime.NewToolRegistry()
	registerReplay(registry, nil)

	handler, ok := registry.Lookup("diverge_replay")
	require.True(t, ok)

	var reqProto []byte
	reqProto = protowire.AppendTag(reqProto, 1, protowire.VarintType)
	reqProto = protowire.AppendVarint(reqProto, 42)
	b64Req := base64.StdEncoding.EncodeToString(reqProto)

	argsJSON, err := json.Marshal(map[string]any{
		"name":        "pr-99",
		"base_url":    srv.URL,
		"routing_key": "pr-99",
		"requests": []map[string]any{
			{"id": "profile", "method": "GET", "path": "/api/v1/profile"},
			{
				"id":          "invoice",
				"method":      "POST",
				"path":        "/acme.v1.Billing/GetInvoice",
				"headers":     map[string]string{"Content-Type": "application/proto"},
				"body_base64": b64Req,
			},
		},
	})
	require.NoError(t, err)

	res, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_replay",
		Arguments: argsJSON,
	})
	require.NoError(t, err)
	assert.True(t, res.IsError, "expected IsError=true because invoice preview dropped protobuf field 2")

	mu.Lock()
	require.True(t, baseReqCaptured)
	require.True(t, prevReqCaptured)
	assert.Equal(t, "POST", baseInvoiceReq.method)
	assert.Equal(t, "application/proto", baseInvoiceReq.contentType)
	assert.Equal(t, reqProto, baseInvoiceReq.body)
	assert.Equal(t, "POST", prevInvoiceReq.method)
	assert.Equal(t, "application/proto", prevInvoiceReq.contentType)
	assert.Equal(t, reqProto, prevInvoiceReq.body)
	mu.Unlock()

	var report replay.Report
	require.NoError(t, json.Unmarshal(res.Content, &report))
	assert.False(t, report.Passed)
	assert.Equal(t, 2, report.TotalRequests)
	assert.Equal(t, 1, report.PassedRequests)
	assert.Equal(t, 1, report.FailedRequests)
	assert.Equal(t, 1, report.BreakingDiffs)

	// Rejects replay when base_url is set without name, routing_key, or preview_url
	missingSelectorArgs, err := json.Marshal(map[string]any{
		"base_url": srv.URL,
		"requests": []map[string]any{
			{"path": "/api/v1/profile"},
		},
	})
	require.NoError(t, err)
	missingRes, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_replay",
		Arguments: missingSelectorArgs,
	})
	require.NoError(t, err)
	assert.True(t, missingRes.IsError)

	// Supports filter_hosts, strip_auth, redact_query_params, and redact_body_fields in MCP tool parameters
	filteredArgs, err := json.Marshal(map[string]any{
		"name":                "pr-99",
		"base_url":            srv.URL,
		"routing_key":         "pr-99",
		"filter_hosts":        []string{"api.example.com"},
		"strip_auth":          true,
		"redact_query_params": []string{"secret_q"},
		"redact_body_fields":  []string{"password"},
		"requests": []map[string]any{
			{"id": "profile", "method": "GET", "path": "/api/v1/profile?token=jwt-1&secret_q=sq-2", "headers": map[string]string{"Authorization": "Bearer secret"}, "body": `{"password":"pw"}`},
			{"id": "thirdparty", "method": "GET", "path": "https://cdn.other.example/bundle.js"},
		},
	})
	require.NoError(t, err)
	filteredRes, err := handler(context.Background(), mcpruntime.ToolRequest{
		ToolName:  "diverge_replay",
		Arguments: filteredArgs,
	})
	require.NoError(t, err)
	assert.False(t, filteredRes.IsError)
	var filteredReport replay.Report
	require.NoError(t, json.Unmarshal(filteredRes.Content, &filteredReport))
	assert.True(t, filteredReport.Passed)
	assert.Equal(t, 1, filteredReport.TotalRequests)
	assert.NotContains(t, filteredReport.Results[0].Path, "jwt-1")
	assert.NotContains(t, filteredReport.Results[0].Path, "sq-2")
	mu.Lock()
	assert.Empty(t, baseProfileAuth)
	assert.Empty(t, prevProfileAuth)
	mu.Unlock()
}

package cli

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/protocgen/proto2mcp/pkg/mcpruntime"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"

	"github.com/divergedev/diverge/pkg/replay"
)

func TestMCPReplay_InlineJSONAndProtobuf(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-99"
		switch r.URL.Path {
		case "/api/v1/profile":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"id":"u-1","plan":"pro"}`))

		case "/acme.v1.Billing/GetInvoice":
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

	var report replay.Report
	require.NoError(t, json.Unmarshal(res.Content, &report))
	assert.False(t, report.Passed)
	assert.Equal(t, 2, report.TotalRequests)
	assert.Equal(t, 1, report.PassedRequests)
	assert.Equal(t, 1, report.FailedRequests)
	assert.Equal(t, 1, report.BreakingDiffs)
}

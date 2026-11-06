package replay

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestParseTrace_JSONL(t *testing.T) {
	raw := `
# Checkout and order trace
{"id":"get-orders","method":"GET","path":"/api/v1/orders"}
// Protobuf POST request
{"method":"POST","path":"/acme.v1.OrderService/CreateOrder","headers":{"Content-Type":"application/proto"},"body_base64":"CgRpdGVt","proto_message":"acme.v1.CreateOrderResponse"}
`
	entries, err := ParseTrace([]byte(raw), ".jsonl")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, "get-orders", entries[0].ID)
	assert.Equal(t, "GET", entries[0].Method)
	assert.Equal(t, "/api/v1/orders", entries[0].Path)

	assert.Equal(t, "req-2", entries[1].ID)
	assert.Equal(t, "POST", entries[1].Method)
	assert.Equal(t, "/acme.v1.OrderService/CreateOrder", entries[1].Path)
	assert.Equal(t, "application/proto", entries[1].Headers["Content-Type"])
	assert.Equal(t, "acme.v1.CreateOrderResponse", entries[1].ProtoMessage)

	payload, err := entries[1].PayloadBytes()
	require.NoError(t, err)
	assert.Equal(t, []byte("\x0a\x04item"), payload)
}

func TestParseTrace_JSONArrayAndWrapper(t *testing.T) {
	arrJSON := `[
		{"path": "/healthz"},
		{"url": "https://api.example.com/v1/Charge", "body": "{\"amount\":100}", "expectStatus": 201}
	]`
	entries, err := ParseTrace([]byte(arrJSON), ".json")
	require.NoError(t, err)
	require.Len(t, entries, 2)
	assert.Equal(t, "GET", entries[0].Method)
	assert.Equal(t, "/healthz", entries[0].Path)
	assert.Equal(t, "POST", entries[1].Method)
	assert.Equal(t, "https://api.example.com/v1/Charge", entries[1].Path)
	assert.Equal(t, 201, entries[1].ExpectStatus)

	wrapperJSON := "{\n\"requests\": [\n{\"method\":\"DELETE\",\"path\":\"/api/v1/session\"}\n]\n}"
	wrapped, err := ParseTrace([]byte(wrapperJSON), ".json")
	require.NoError(t, err)
	require.Len(t, wrapped, 1)
	assert.Equal(t, "DELETE", wrapped[0].Method)
	assert.Equal(t, "/api/v1/session", wrapped[0].Path)
}

func TestParseTrace_HAR(t *testing.T) {
	protoBytes := []byte{0x08, 0x2a, 0x12, 0x05, 'h', 'e', 'l', 'l', 'o'}
	b64 := base64.StdEncoding.EncodeToString(protoBytes)

	harJSON := `{
		"log": {
			"version": "1.2",
			"entries": [
				{
					"request": {
						"method": "GET",
						"url": "https://staging.example.com/api/v1/catalog",
						"headers": [
							{"name": ":authority", "value": "staging.example.com"},
							{"name": "Accept", "value": "application/json"},
							{"name": "Host", "value": "staging.example.com"}
						]
					}
				},
				{
					"request": {
						"method": "POST",
						"url": "https://staging.example.com/acme.v1.Catalog/GetItem",
						"headers": [
							{"name": "Connect-Protocol-Version", "value": "1"}
						],
						"postData": {
							"mimeType": "application/proto",
							"text": "` + b64 + `",
							"encoding": "base64"
						}
					}
				}
			]
		}
	}`

	entries, err := ParseTrace([]byte(harJSON), ".har")
	require.NoError(t, err)
	require.Len(t, entries, 2)

	assert.Equal(t, "har-1", entries[0].ID)
	assert.Equal(t, "GET", entries[0].Method)
	assert.Equal(t, "https://staging.example.com/api/v1/catalog", entries[0].Path)
	assert.Equal(t, "application/json", entries[0].Headers["Accept"])
	assert.NotContains(t, entries[0].Headers, ":authority")
	assert.NotContains(t, entries[0].Headers, "Host")

	assert.Equal(t, "har-2", entries[1].ID)
	assert.Equal(t, "POST", entries[1].Method)
	assert.Equal(t, "application/proto", entries[1].Headers["Content-Type"])
	decoded, err := entries[1].PayloadBytes()
	require.NoError(t, err)
	assert.Equal(t, protoBytes, decoded)
}

func TestLoadTrace_FileAndErrors(t *testing.T) {
	tmpDir := t.TempDir()
	tracePath := filepath.Join(tmpDir, "trace.jsonl")
	require.NoError(t, os.WriteFile(tracePath, []byte(`{"path":"/v1/ping"}`+"\n"), 0o644))

	entries, err := LoadTrace(tracePath)
	require.NoError(t, err)
	require.Len(t, entries, 1)
	assert.Equal(t, "/v1/ping", entries[0].Path)

	_, err = LoadTrace("")
	assert.Error(t, err)

	_, err = ParseTrace([]byte("   "), ".jsonl")
	assert.Error(t, err)

	_, err = ParseTrace([]byte(`{"method":"GET"}`), ".jsonl")
	assert.ErrorContains(t, err, "missing path or url")
}

package cli

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/replay"
)

func TestReplayCmd_JSONAndProtobufPassAndFail(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-42"

		switch r.URL.Path {
		case "/api/v1/checkout":
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusOK)
			if isPreview {
				_, _ = w.Write([]byte(`{"order_id":"ord-2","status":"confirmed","amount":1999}`))
			} else {
				_, _ = w.Write([]byte(`{"order_id":"ord-1","status":"confirmed","amount":1999}`))
			}

		case "/acme.v1.Inventory/CheckStock":
			w.Header().Set("Content-Type", "application/proto")
			var msg []byte
			msg = protowire.AppendTag(msg, 1, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 1) // in_stock = true
			msg = protowire.AppendTag(msg, 2, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 25) // available_qty = 25
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(msg)

		case "/acme.v1.Inventory/BrokenRPC":
			w.Header().Set("Content-Type", "application/proto")
			var msg []byte
			msg = protowire.AppendTag(msg, 1, protowire.VarintType)
			msg = protowire.AppendVarint(msg, 1)
			if !isPreview {
				// Baseline includes field 2, preview omits it
				msg = protowire.AppendTag(msg, 2, protowire.BytesType)
				msg = protowire.AppendString(msg, "warehouse-west")
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write(msg)

		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, divergeiov1alpha1.AddToScheme(scheme))

	env := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-42",
			Namespace: "default",
		},
		Spec: divergeiov1alpha1.EnvironmentSpec{
			Routing: divergeiov1alpha1.EnvironmentRouting{
				Mode:        "header",
				HeaderKey:   "x-diverge-env",
				HeaderValue: "pr-42",
			},
		},
		Status: divergeiov1alpha1.EnvironmentStatus{
			Phase: divergeiov1alpha1.PhaseRunning,
			URL:   srv.URL,
		},
	}

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).Build()
	app := &App{
		Namespace: "default",
		Client:    fakeClient,
	}

	var reqProto []byte
	reqProto = protowire.AppendTag(reqProto, 1, protowire.BytesType)
	reqProto = protowire.AppendString(reqProto, "sku-100")
	reqB64 := base64.StdEncoding.EncodeToString(reqProto)

	tmpDir := t.TempDir()
	passTracePath := filepath.Join(tmpDir, "pass.jsonl")
	passTrace := `{"id":"checkout","method":"POST","path":"/api/v1/checkout","body":"{\"sku\":\"sku-100\"}"}` + "\n" +
		`{"id":"stock-proto","method":"POST","path":"/acme.v1.Inventory/CheckStock","headers":{"Content-Type":"application/proto"},"body_base64":"` + reqB64 + `"}` + "\n"
	require.NoError(t, os.WriteFile(passTracePath, []byte(passTrace), 0o644))

	// 1. Passing replay with JSON output
	root := NewRootCmd(app)
	var outBuf bytes.Buffer
	root.SetOut(&outBuf)
	root.SetErr(&outBuf)
	root.SetArgs([]string{"replay", "pr-42", "--from", passTracePath, "--json"})

	err := root.Execute()
	require.NoError(t, err)

	var report replay.Report
	require.NoError(t, json.Unmarshal(outBuf.Bytes(), &report))
	assert.True(t, report.Passed)
	assert.Equal(t, 2, report.TotalRequests)
	assert.Equal(t, 2, report.PassedRequests)
	assert.Equal(t, replay.ProtocolJSON, report.Results[0].Protocol)
	assert.Equal(t, replay.ProtocolProtobuf, report.Results[1].Protocol)

	// 2. Failing replay when Protobuf wire field is dropped in preview
	failTracePath := filepath.Join(tmpDir, "fail.jsonl")
	failTrace := `{"id":"broken-proto","method":"POST","path":"/acme.v1.Inventory/BrokenRPC","headers":{"Content-Type":"application/proto"}}` + "\n"
	require.NoError(t, os.WriteFile(failTracePath, []byte(failTrace), 0o644))

	summaryFile := filepath.Join(tmpDir, "step-summary.md")
	t.Setenv("GITHUB_STEP_SUMMARY", summaryFile)

	rootFail := NewRootCmd(app)
	var failOut bytes.Buffer
	rootFail.SetOut(&failOut)
	rootFail.SetErr(&failOut)
	rootFail.SetArgs([]string{"replay", "pr-42", "--from", failTracePath, "--github-summary"})

	err = rootFail.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "traffic replay failed for environment pr-42")
	assert.Contains(t, failOut.String(), "[FAIL]")

	summaryBytes, err := os.ReadFile(summaryFile)
	require.NoError(t, err)
	assert.Contains(t, string(summaryBytes), "Diverge Traffic Replay Failed: pr-42")
	assert.Contains(t, string(summaryBytes), "`proto.field_2`")
}

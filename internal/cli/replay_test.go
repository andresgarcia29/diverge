package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"github.com/spf13/cobra"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/protobuf/encoding/protowire"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
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

		case "/api/v1/status-diff":
			w.Header().Set("Content-Type", "application/json")
			if isPreview {
				w.WriteHeader(http.StatusServiceUnavailable)
			} else {
				w.WriteHeader(http.StatusOK)
			}
			_, _ = w.Write([]byte(`{"status":"identical"}`))

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

	// 3. Status mismatch fails by default and passes when --fail-on-status-diff=false
	statusTracePath := filepath.Join(tmpDir, "status-diff.jsonl")
	require.NoError(t, os.WriteFile(statusTracePath, []byte(`{"id":"status-diff","method":"GET","path":"/api/v1/status-diff"}`+"\n"), 0o644))

	rootStatusFail := NewRootCmd(app)
	var statusFailOut bytes.Buffer
	rootStatusFail.SetOut(&statusFailOut)
	rootStatusFail.SetErr(&statusFailOut)
	rootStatusFail.SetArgs([]string{"replay", "pr-42", "--from", statusTracePath})
	require.Error(t, rootStatusFail.Execute())

	rootStatusPass := NewRootCmd(app)
	var statusPassOut, statusPassErr bytes.Buffer
	t.Setenv("GITHUB_STEP_SUMMARY", "")
	rootStatusPass.SetOut(&statusPassOut)
	rootStatusPass.SetErr(&statusPassErr)
	rootStatusPass.SetArgs([]string{"replay", "pr-42", "--from", statusTracePath, "--fail-on-status-diff=false", "--json", "--github-summary"})
	require.NoError(t, rootStatusPass.Execute())

	var statusReport replay.Report
	require.NoError(t, json.Unmarshal(statusPassOut.Bytes(), &statusReport))
	assert.True(t, statusReport.Passed)
	assert.Contains(t, statusPassErr.String(), "Diverge Traffic Replay Passed: pr-42")
}

func TestSelectPreviewGroupServiceURL(t *testing.T) {
	webAdminHash := fmt.Sprintf("%x", sha256.Sum256([]byte("pr-42/web-admin")))[:8]
	webAdminChildEnv := "pg-pr-42-web-admin-" + webAdminHash

	overlappingServices := []divergeiov1alpha1.PreviewGroupServiceStatus{
		{
			Name: "web",
			URL:  "https://web-preview.example.com",
		},
		{
			Name: "web-admin",
			URL:  "https://web-admin-preview.example.com",
		},
	}

	distinctServices := []divergeiov1alpha1.PreviewGroupServiceStatus{
		{
			Name: "api",
			URL:  "https://api-preview.example.com",
		},
		{
			Name: "worker",
			URL:  "https://worker-preview.example.com",
		},
	}

	tests := []struct {
		name            string
		services        []divergeiov1alpha1.PreviewGroupServiceStatus
		explicitBaseURL string
		externalURL     string
		entries         []replay.RequestEntry
		wantURL         string
		wantAmbiguous   bool
	}{
		{
			name: "single service with URL is unambiguous",
			services: []divergeiov1alpha1.PreviewGroupServiceStatus{
				{Name: "api", URL: "https://api-preview.example.com"},
			},
			wantURL:       "https://api-preview.example.com",
			wantAmbiguous: false,
		},
		{
			name:            "distinct service matched by base URL host",
			services:        distinctServices,
			explicitBaseURL: "https://pr-42-api.example.com",
			wantURL:         "https://api-preview.example.com",
			wantAmbiguous:   false,
		},
		{
			name:          "distinct service matched by absolute trace entry",
			services:      distinctServices,
			entries:       []replay.RequestEntry{{Method: "GET", Path: "https://worker.staging.example.com/health"}},
			wantURL:       "https://worker-preview.example.com",
			wantAmbiguous: false,
		},
		{
			name:            "overlapping service names web and web-admin on pr-42-web-admin.example.com returns ambiguous",
			services:        overlappingServices,
			explicitBaseURL: "https://pr-42-web-admin.example.com",
			wantURL:         "",
			wantAmbiguous:   true,
		},
		{
			name:            "overlapping service names resolved by URL template",
			services:        overlappingServices,
			explicitBaseURL: "https://web-admin.staging.example.com",
			externalURL:     "https://{env}.staging.example.com",
			wantURL:         "https://web-admin-preview.example.com",
			wantAmbiguous:   false,
		},
		{
			name:            "overlapping service names resolved by generated child environment name",
			services:        overlappingServices,
			explicitBaseURL: "https://" + webAdminChildEnv + ".preview.example.com",
			wantURL:         "https://web-admin-preview.example.com",
			wantAmbiguous:   false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			gotURL, gotAmbiguous := selectPreviewGroupServiceURL(tt.services, tt.explicitBaseURL, tt.externalURL, tt.entries)
			assert.Equal(t, tt.wantURL, gotURL)
			assert.Equal(t, tt.wantAmbiguous, gotAmbiguous)
		})
	}
}

func TestResolveReplayRouting(t *testing.T) {
	scheme := runtime.NewScheme()
	require.NoError(t, divergeiov1alpha1.AddToScheme(scheme))

	relEntries := []replay.RequestEntry{{Method: "GET", Path: "/api/v1/items"}}
	absEntries := []replay.RequestEntry{{Method: "GET", Path: "https://staging.example.com/api/v1/items"}}

	subdomainEnv := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-env", Namespace: "default"},
		Spec: divergeiov1alpha1.EnvironmentSpec{
			Routing: divergeiov1alpha1.EnvironmentRouting{Mode: "subdomain"},
		},
		Status: divergeiov1alpha1.EnvironmentStatus{
			URL: "https://sub-env.preview.example.com",
		},
	}

	subdomainEnvNoURL := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{Name: "sub-pending", Namespace: "default"},
		Spec: divergeiov1alpha1.EnvironmentSpec{
			Routing: divergeiov1alpha1.EnvironmentRouting{Mode: "subdomain"},
		},
	}

	headerMultiPG := &divergeiov1alpha1.PreviewGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-header", Namespace: "default"},
		Spec: divergeiov1alpha1.PreviewGroupSpec{
			Routing: divergeiov1alpha1.PreviewGroupRouting{
				Mode:        "header",
				HeaderKey:   "x-custom-env",
				HeaderValue: "pg-header-val",
			},
		},
		Status: divergeiov1alpha1.PreviewGroupStatus{
			Services: []divergeiov1alpha1.PreviewGroupServiceStatus{
				{Name: "web", URL: "https://web.preview.example.com"},
				{Name: "web-admin", URL: "https://web-admin.preview.example.com"},
			},
		},
	}

	subdomainMultiPG := &divergeiov1alpha1.PreviewGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-sub", Namespace: "default"},
		Spec: divergeiov1alpha1.PreviewGroupSpec{
			Routing: divergeiov1alpha1.PreviewGroupRouting{Mode: "subdomain"},
		},
		Status: divergeiov1alpha1.PreviewGroupStatus{
			Services: []divergeiov1alpha1.PreviewGroupServiceStatus{
				{Name: "web", URL: "https://web.preview.example.com"},
				{Name: "web-admin", URL: "https://web-admin.preview.example.com"},
			},
		},
	}

	tests := []struct {
		name           string
		objects        []client.Object
		nilApp         bool
		envName        string
		entries        []replay.RequestEntry
		opts           replayOptions
		wantBaseURL    string
		wantPreviewURL string
		wantHeaderKey  string
		wantHeaderVal  string
		wantErrSubstr  string
	}{
		{
			name:           "subdomain environment with explicit base-url separates baseline and preview",
			objects:        []client.Object{subdomainEnv},
			envName:        "sub-env",
			entries:        relEntries,
			opts:           replayOptions{baseURL: "https://staging.example.com"},
			wantBaseURL:    "https://staging.example.com",
			wantPreviewURL: "https://sub-env.preview.example.com",
			wantHeaderKey:  replay.DefaultHeaderKey,
			wantHeaderVal:  "sub-env",
		},
		{
			name:           "subdomain environment with absolute-URL trace does not require base-url",
			objects:        []client.Object{subdomainEnv},
			envName:        "sub-env",
			entries:        absEntries,
			opts:           replayOptions{},
			wantBaseURL:    "",
			wantPreviewURL: "https://sub-env.preview.example.com",
			wantHeaderKey:  replay.DefaultHeaderKey,
			wantHeaderVal:  "sub-env",
		},
		{
			name:          "subdomain environment without base-url and relative trace fails",
			objects:       []client.Object{subdomainEnv},
			envName:       "sub-env",
			entries:       relEntries,
			opts:          replayOptions{},
			wantErrSubstr: "pass --base-url <baseline-url>",
		},
		{
			name:          "subdomain environment without published preview URL fails",
			objects:       []client.Object{subdomainEnvNoURL},
			envName:       "sub-pending",
			entries:       absEntries,
			opts:          replayOptions{},
			wantErrSubstr: "has no published preview URL",
		},
		{
			name:           "header-routed PreviewGroup with multiple services succeeds when base-url is provided",
			objects:        []client.Object{headerMultiPG},
			envName:        "pg-header",
			entries:        relEntries,
			opts:           replayOptions{baseURL: "https://staging.example.com"},
			wantBaseURL:    "https://staging.example.com",
			wantPreviewURL: "https://staging.example.com",
			wantHeaderKey:  "x-custom-env",
			wantHeaderVal:  "pg-header-val",
		},
		{
			name:          "subdomain-routed PreviewGroup with ambiguous overlapping services fails without preview-url",
			objects:       []client.Object{subdomainMultiPG},
			envName:       "pg-sub",
			entries:       relEntries,
			opts:          replayOptions{baseURL: "https://pr-42-web-admin.example.com"},
			wantErrSubstr: "has multiple service URLs; pass --preview-url",
		},
		{
			name:           "canSkipCluster allows replay without cluster when base-url and header-value are set",
			nilApp:         true,
			envName:        "offline-env",
			entries:        relEntries,
			opts:           replayOptions{baseURL: "https://staging.example.com", headerValue: "offline-env"},
			wantBaseURL:    "https://staging.example.com",
			wantPreviewURL: "https://staging.example.com",
			wantHeaderKey:  replay.DefaultHeaderKey,
			wantHeaderVal:  "offline-env",
		},
		{
			name:          "missing cluster fails when routing flags are omitted",
			nilApp:        true,
			envName:       "offline-env",
			entries:       relEntries,
			opts:          replayOptions{baseURL: "https://staging.example.com"},
			wantErrSubstr: "cannot resolve routing",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var app *App
			if !tt.nilApp {
				fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(tt.objects...).Build()
				app = &App{Namespace: "default", Client: fc}
			}
			cmd := &cobra.Command{}
			cmd.SetContext(context.Background())

			opts := tt.opts
			baseURL, previewURL, headerKey, headerVal, _, err := resolveReplayRouting(cmd, app, tt.envName, tt.entries, &opts)
			if tt.wantErrSubstr != "" {
				require.Error(t, err)
				assert.Contains(t, err.Error(), tt.wantErrSubstr)
				return
			}
			require.NoError(t, err)
			assert.Equal(t, tt.wantBaseURL, baseURL)
			assert.Equal(t, tt.wantPreviewURL, previewURL)
			assert.Equal(t, tt.wantHeaderKey, headerKey)
			assert.Equal(t, tt.wantHeaderVal, headerVal)
		})
	}
}

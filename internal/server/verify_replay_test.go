package server

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
)

func TestVerifyReplayHandler_VerifyAndReplayWithAutoFix(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		isPreview := r.Header.Get("x-diverge-env") == "pr-42"
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte(`{"ok":true}`))
		case "/api/v1/orders":
			w.WriteHeader(http.StatusOK)
			if isPreview {
				// Preview drops "currency" field -> breaking contract diff
				_, _ = w.Write([]byte(`{"id":"ord-1","total":1999}`))
			} else {
				_, _ = w.Write([]byte(`{"id":"ord-1","total":1999,"currency":"USD"}`))
			}
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	}))
	defer srv.Close()

	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, divergeiov1alpha1.AddToScheme(scheme))

	env := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-42",
			Namespace: "default",
		},
		Spec: divergeiov1alpha1.EnvironmentSpec{
			Source: divergeiov1alpha1.EnvironmentSource{
				Provider: "github",
				Project:  "acme/orders",
				Branch:   "feat/orders",
				MR:       42,
			},
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

	fc := fake.NewClientBuilder().WithScheme(scheme).WithObjects(env).Build()
	handler := NewVerifyReplayHandler(fc, nil, nil, nil)
	handler.httpClient = srv.Client()

	// 1. POST /api/v1/environments/verify passes for /healthz
	verifyPayload, err := json.Marshal(VerifyEnvironmentRequest{
		Name:            "pr-42",
		Namespace:       "default",
		Paths:           []string{"/healthz"},
		CompareBaseline: true,
	})
	require.NoError(t, err)

	reqVerify := httptest.NewRequest(http.MethodPost, "/api/v1/environments/verify", bytes.NewReader(verifyPayload))
	recVerify := httptest.NewRecorder()
	handler.HandleVerify(recVerify, reqVerify)
	require.Equal(t, http.StatusOK, recVerify.Code)

	var verifyResp VerifyEnvironmentResponse
	require.NoError(t, json.Unmarshal(recVerify.Body.Bytes(), &verifyResp))
	assert.True(t, verifyResp.Passed)
	assert.Equal(t, "pr-42", verifyResp.Environment)
	require.Len(t, verifyResp.Checks, 2)

	// 2. POST /api/v1/environments/replay detects breaking contract diff and triggers AutoFix AgentTask
	replayPayload, err := json.Marshal(ReplayEnvironmentRequest{
		Name:         "pr-42",
		Namespace:    "default",
		TraceContent: `{"id":"orders","method":"GET","path":"/api/v1/orders?token=secret-jwt"}` + "\n",
		TraceFormat:  ".jsonl",
		AutoFix:      true,
	})
	require.NoError(t, err)

	reqReplay := httptest.NewRequest(http.MethodPost, "/api/v1/environments/replay", bytes.NewReader(replayPayload))
	recReplay := httptest.NewRecorder()
	handler.HandleReplay(recReplay, reqReplay)
	require.Equal(t, http.StatusOK, recReplay.Code)

	var replayResp ReplayEnvironmentResponse
	require.NoError(t, json.Unmarshal(recReplay.Body.Bytes(), &replayResp))
	assert.False(t, replayResp.Passed)
	assert.Equal(t, 1, replayResp.BreakingDiffs)
	assert.Equal(t, "autofix-replay-pr-42", replayResp.AutoFixTaskName)
	assert.NotContains(t, replayResp.Results[0].Path, "secret-jwt")

	var task divergeiov1alpha1.AgentTask
	require.NoError(t, fc.Get(context.Background(), client.ObjectKey{Name: "autofix-replay-pr-42", Namespace: "default"}, &task))
	assert.Equal(t, "https://github.com/acme/orders", task.Spec.Repository.URL)
	assert.Contains(t, task.Spec.Objective, "currency")

	// 3. Rejects unauthorized target URL host (SSRF defense)
	ssrfPayload, err := json.Marshal(VerifyEnvironmentRequest{
		Name:       "pr-42",
		Namespace:  "default",
		PreviewURL: "https://internal-db.corp.local:5432",
	})
	require.NoError(t, err)
	reqSSRF := httptest.NewRequest(http.MethodPost, "/api/v1/environments/verify", bytes.NewReader(ssrfPayload))
	recSSRF := httptest.NewRecorder()
	handler.HandleVerify(recSSRF, reqSSRF)
	assert.Equal(t, http.StatusBadRequest, recSSRF.Code)

	// 4. Rejects GET method with 405
	reqGet := httptest.NewRequest(http.MethodGet, "/api/v1/environments/verify", nil)
	recGet := httptest.NewRecorder()
	handler.HandleVerify(recGet, reqGet)
	assert.Equal(t, http.StatusMethodNotAllowed, recGet.Code)
}

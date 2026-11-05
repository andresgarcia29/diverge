package server

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/divergedev/diverge/api/v1alpha1"
	pkgauth "github.com/divergedev/diverge/pkg/auth"
)

func setupLLMMeterTest(t *testing.T) (client.Client, *pkgauth.MacaroonProvider, *pkgauth.InMemoryTokenStore, *LLMMeterHandler) {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	crdClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentTask{}).
		Build()

	rootKey := []byte("0123456789abcdef0123456789abcdef")
	verifier := pkgauth.NewMacaroonProvider(rootKey)
	store := pkgauth.NewInMemoryTokenStore()
	handler := NewLLMMeterHandler(crdClient, verifier, store, slog.Default())
	return crdClient, verifier, store, handler
}

func mintTestMeterToken(t *testing.T, verifier *pkgauth.MacaroonProvider, store *pkgauth.InMemoryTokenStore, claims pkgauth.Claims) string {
	t.Helper()
	ctx := context.Background()
	tok, err := verifier.Mint(ctx, claims)
	require.NoError(t, err)
	raw, err := tok.Serialize()
	require.NoError(t, err)
	require.NoError(t, store.SaveToken(ctx, claims.TaskID, claims.Namespace, raw))
	return base64.RawStdEncoding.EncodeToString(raw)
}

func TestLLMMeterHandler_ValidMetering(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-meter-ok",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:    "Implement feature",
			BudgetUSD:    "5.00",
			BudgetTokens: 100000,
			Capabilities: []string{"gemini-2.5-pro", "gemini-2.5-flash"},
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
		Status: v1alpha1.AgentTaskStatus{
			Phase: v1alpha1.AgentTaskPhaseActive,
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))
	task.Status.Phase = v1alpha1.AgentTaskPhaseActive
	require.NoError(t, crdClient.Status().Update(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:        "task-meter-ok",
		Namespace:     "default",
		AllowedModels: []string{"gemini-2.5-pro", "gemini-2.5-flash"},
		MaxCostUSD:    5.00,
		MaxTokens:     100000,
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	// First metering call
	body1, _ := json.Marshal(LLMMeterRequest{
		Model:      "gemini-2.5-pro",
		TokensUsed: 12000,
		CostUSD:    0.45,
	})
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)

	require.Equal(t, http.StatusOK, rec1.Code)
	var resp1 LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&resp1))
	assert.True(t, resp1.Allowed)
	assert.False(t, resp1.Suspended)
	assert.Equal(t, int64(12000), resp1.TotalTokensUsed)
	assert.InDelta(t, 0.45, resp1.TotalCostUSD, 0.0001)

	// Second metering call accumulates onto previous usage
	body2, _ := json.Marshal(LLMMeterRequest{
		Model:      "gemini-2.5-flash",
		TokensUsed: 8000,
		CostUSD:    0.15,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)

	require.Equal(t, http.StatusOK, rec2.Code)
	var resp2 LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&resp2))
	assert.True(t, resp2.Allowed)
	assert.False(t, resp2.Suspended)
	assert.Equal(t, int64(20000), resp2.TotalTokensUsed)
	assert.InDelta(t, 0.60, resp2.TotalCostUSD, 0.0001)

	// Verify CRD status was updated
	var updated v1alpha1.AgentTask
	require.NoError(t, crdClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "task-meter-ok"}, &updated))
	assert.Equal(t, int64(20000), updated.Status.TokensConsumed)
	assert.Equal(t, "0.6000", updated.Status.CostUSD)
	assert.False(t, updated.Spec.Suspended)
	assert.Equal(t, v1alpha1.AgentTaskPhaseActive, updated.Status.Phase)
}

func TestLLMMeterHandler_DisallowedModel(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-model-check",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:    "Use lite model only",
			Capabilities: []string{"gemini-2.5-flash"},
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:        "task-model-check",
		Namespace:     "default",
		AllowedModels: []string{"gemini-2.5-flash"},
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	body, _ := json.Marshal(LLMMeterRequest{
		Model:      "claude-opus-4",
		TokensUsed: 1000,
		CostUSD:    0.10,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusForbidden, rec.Code)
	var resp LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Allowed)
	assert.Contains(t, resp.Reason, "claude-opus-4")

	// Empty model when AllowedModels is restricted must also be rejected
	emptyModelBody, _ := json.Marshal(LLMMeterRequest{
		Model:      "   ",
		TokensUsed: 1000,
		CostUSD:    0.10,
	})
	emptyReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(emptyModelBody))
	emptyReq.Header.Set("Authorization", "Bearer "+token)
	emptyRec := httptest.NewRecorder()
	handler.ServeHTTP(emptyRec, emptyReq)

	require.Equal(t, http.StatusForbidden, emptyRec.Code)
	var emptyResp LLMMeterResponse
	require.NoError(t, json.NewDecoder(emptyRec.Body).Decode(&emptyResp))
	assert.False(t, emptyResp.Allowed)
	assert.Equal(t, "model is required when token restricts allowed_models", emptyResp.Reason)
}

func TestLLMMeterHandler_BudgetExceededAutoPauseAndRevoke(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-budget-cap",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:    "Test budget enforcement",
			BudgetUSD:    "1.00",
			BudgetTokens: 50000,
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))
	task.Status.Phase = v1alpha1.AgentTaskPhaseActive
	task.Status.CostUSD = "0.7500"
	task.Status.TokensConsumed = 30000
	require.NoError(t, crdClient.Status().Update(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:     "task-budget-cap",
		Namespace:  "default",
		MaxCostUSD: 1.00,
		MaxTokens:  50000,
		ExpiresAt:  time.Now().Add(time.Hour),
	})

	// Push cumulative cost to $1.25 (> $1.00 budget)
	body, _ := json.Marshal(LLMMeterRequest{
		Model:      "gemini-2.5-pro",
		TokensUsed: 10000,
		CostUSD:    0.50,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)

	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	var resp LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Allowed)
	assert.True(t, resp.Suspended)
	assert.Equal(t, "budget exceeded", resp.Reason)
	assert.Equal(t, int64(40000), resp.TotalTokensUsed)
	assert.InDelta(t, 1.25, resp.TotalCostUSD, 0.0001)

	// Verify AgentTask CR is paused & suspended
	var updated v1alpha1.AgentTask
	require.NoError(t, crdClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "task-budget-cap"}, &updated))
	assert.True(t, updated.Spec.Suspended)
	assert.Equal(t, v1alpha1.AgentTaskPhasePaused, updated.Status.Phase)
	assert.Equal(t, int64(40000), updated.Status.TokensConsumed)
	assert.Equal(t, "1.2500", updated.Status.CostUSD)

	// Verify token was revoked in TokenStore
	assert.True(t, store.IsRevoked(ctx, "task-budget-cap", "default"))

	// Subsequent request with the same token must be rejected with 401 Unauthorized
	reqAfter := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body))
	reqAfter.Header.Set("Authorization", "Bearer "+token)
	recAfter := httptest.NewRecorder()
	handler.ServeHTTP(recAfter, reqAfter)
	assert.Equal(t, http.StatusUnauthorized, recAfter.Code)
}

func TestLLMMeterHandler_RevokedAndNegativeInputs(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-revoked",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Test revoked token",
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:    "task-revoked",
		Namespace: "default",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	// Negative tokensUsed -> 400 Bad Request
	negBody, _ := json.Marshal(LLMMeterRequest{
		TokensUsed: -100,
		CostUSD:    0.01,
	})
	negReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(negBody))
	negReq.Header.Set("Authorization", "Bearer "+token)
	negRec := httptest.NewRecorder()
	handler.ServeHTTP(negRec, negReq)
	assert.Equal(t, http.StatusBadRequest, negRec.Code)

	// Revoke token -> 401 Unauthorized
	require.NoError(t, store.RevokeToken(ctx, "task-revoked", "default"))
	okBody, _ := json.Marshal(LLMMeterRequest{
		TokensUsed: 100,
		CostUSD:    0.01,
	})
	revReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(okBody))
	revReq.Header.Set("Authorization", "Bearer "+token)
	revRec := httptest.NewRecorder()
	handler.ServeHTTP(revRec, revReq)
	assert.Equal(t, http.StatusUnauthorized, revRec.Code)
}

func TestLLMMeterHandler_TokensOverflowAndConflictRetry(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	require.NoError(t, corev1.AddToScheme(scheme))

	var statusUpdateAttempts atomic.Int32
	crdClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithStatusSubresource(&v1alpha1.AgentTask{}).
		WithInterceptorFuncs(interceptor.Funcs{
			SubResourceUpdate: func(ctx context.Context, c client.Client, subResourceName string, obj client.Object, opts ...client.SubResourceUpdateOption) error {
				if subResourceName == "status" && statusUpdateAttempts.Add(1) == 1 {
					return apierrors.NewConflict(schema.GroupResource{Group: "divergedev.com", Resource: "agenttasks"}, obj.GetName(), nil)
				}
				return c.Status().Update(ctx, obj, opts...)
			},
		}).
		Build()

	rootKey := []byte("0123456789abcdef0123456789abcdef")
	verifier := pkgauth.NewMacaroonProvider(rootKey)
	store := pkgauth.NewInMemoryTokenStore()
	handler := NewLLMMeterHandler(crdClient, verifier, store, slog.Default())

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-overflow-retry",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Test overflow and conflict retry",
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:    "task-overflow-retry",
		Namespace: "default",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	// 1. First request succeeds after retrying the initial 409 Conflict
	body1, _ := json.Marshal(LLMMeterRequest{
		TokensUsed: 500,
		CostUSD:    0.05,
	})
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	assert.GreaterOrEqual(t, statusUpdateAttempts.Load(), int32(2))

	// 2. Overflow request (500 + MaxInt64 > MaxInt64) is rejected with 400 Bad Request
	overflowBody, _ := json.Marshal(LLMMeterRequest{
		TokensUsed: math.MaxInt64,
		CostUSD:    0.01,
	})
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(overflowBody))
	req2.Header.Set("Authorization", "Bearer "+token)
	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, req2)
	require.Equal(t, http.StatusBadRequest, rec2.Code)
	var overflowResp LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec2.Body).Decode(&overflowResp))
	assert.False(t, overflowResp.Allowed)
	assert.Equal(t, "tokensUsed overflows accumulated usage", overflowResp.Reason)
}

func TestLLMMeterHandler_PerTaskLocking(t *testing.T) {
	_, _, _, handler := setupLLMMeterTest(t)

	muA1 := handler.taskLock("default", "task-a")
	muA2 := handler.taskLock("default", "task-a")
	muB := handler.taskLock("default", "task-b")
	muOtherNS := handler.taskLock("staging", "task-a")

	assert.Same(t, muA1, muA2, "same namespace and taskID should return identical mutex")
	assert.NotSame(t, muA1, muB, "different taskIDs should use distinct mutexes")
	assert.NotSame(t, muA1, muOtherNS, "different namespaces should use distinct mutexes")
}

func TestLLMMeterHandler_IdempotencyWildcardIterationAndGuidance(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-idempotent",
			Namespace: "default",
			Annotations: map[string]string{
				"divergedev.com/guidance": "Use mock database in unit tests",
			},
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:     "Test idempotency and wildcard model",
			BudgetUSD:     "5.00",
			BudgetTokens:  100000,
			MaxIterations: 2,
			Capabilities:  []string{"claude-sonnet-4*"},
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:        "task-idempotent",
		Namespace:     "default",
		AllowedModels: []string{"claude-sonnet-4*"},
		MaxCostUSD:    5.00,
		MaxTokens:     100000,
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	// 1. Wildcard model "claude-sonnet-4-20250514" with RequestID "req-1" and Iteration 1
	body1, _ := json.Marshal(LLMMeterRequest{
		RequestID:  "req-1",
		Iteration:  1,
		Model:      "claude-sonnet-4-20250514",
		TokensUsed: 5000,
		CostUSD:    0.20,
	})
	req1 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer "+token)
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, req1)
	require.Equal(t, http.StatusOK, rec1.Code)
	var resp1 LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec1.Body).Decode(&resp1))
	assert.True(t, resp1.Allowed)
	assert.Equal(t, int64(5000), resp1.TotalTokensUsed)
	assert.InDelta(t, 0.20, resp1.TotalCostUSD, 0.0001)
	assert.Equal(t, int32(1), resp1.Iteration)
	assert.Equal(t, int32(2), resp1.MaxIterations)
	assert.Equal(t, "Use mock database in unit tests", resp1.Guidance)

	// 2. Duplicate RequestID "req-1" is idempotent and does NOT double-count tokens/cost/iteration
	reqDup := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body1))
	reqDup.Header.Set("Authorization", "Bearer "+token)
	recDup := httptest.NewRecorder()
	handler.ServeHTTP(recDup, reqDup)
	require.Equal(t, http.StatusOK, recDup.Code)
	var respDup LLMMeterResponse
	require.NoError(t, json.NewDecoder(recDup.Body).Decode(&respDup))
	assert.Equal(t, int64(5000), respDup.TotalTokensUsed)
	assert.InDelta(t, 0.20, respDup.TotalCostUSD, 0.0001)
	assert.Equal(t, int32(1), respDup.Iteration)

	// 3. Exceeding MaxIterations (Iteration 3 > MaxIterations 2) pauses and suspends task
	body3, _ := json.Marshal(LLMMeterRequest{
		RequestID:  "req-3",
		Iteration:  3,
		Model:      "claude-sonnet-4-20250514",
		TokensUsed: 5000,
		CostUSD:    0.20,
	})
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body3))
	req3.Header.Set("Authorization", "Bearer "+token)
	rec3 := httptest.NewRecorder()
	handler.ServeHTTP(rec3, req3)
	require.Equal(t, http.StatusTooManyRequests, rec3.Code)
	var resp3 LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec3.Body).Decode(&resp3))
	assert.False(t, resp3.Allowed)
	assert.True(t, resp3.Suspended)
	assert.Equal(t, "max iterations exceeded", resp3.Reason)
}

func TestLLMMeterHandler_MultiCallSameIterationAndSpecBudgetAuthority(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-multi-call-iter",
			Namespace: "default",
			Annotations: map[string]string{
				"divergedev.com/preview-url": "https://pr-42.preview.diverge.dev",
			},
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:     "Multi-call per iteration and live budget bump",
			BudgetUSD:     "10.00", // Live spec bumped from initial $2.00 token caveat
			BudgetTokens:  200000,
			MaxIterations: 3,
			Capabilities:  []string{"fast"},
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))
	task.Status.Phase = v1alpha1.AgentTaskPhaseActive
	require.NoError(t, crdClient.Status().Update(ctx, task))

	// Token minted with older $2.00 caveat and capability tier alias "fast"
	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:        "task-multi-call-iter",
		Namespace:     "default",
		AllowedModels: []string{"fast"},
		MaxCostUSD:    2.00,
		MaxTokens:     50000,
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	// Three LLM sub-calls all belonging to Iteration 1, totaling $3.00 (> $2.00 token caveat, <= $10.00 live spec)
	for i := 1; i <= 3; i++ {
		body, _ := json.Marshal(LLMMeterRequest{
			Iteration:  1,
			Model:      "gemini-2.5-flash",
			TokensUsed: 10000,
			CostUSD:    1.00,
		})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body))
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		require.Equal(t, http.StatusOK, rec.Code)

		var resp LLMMeterResponse
		require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
		assert.True(t, resp.Allowed)
		assert.Equal(t, int32(1), resp.Iteration, "multiple calls with Iteration=1 must not increment past 1")
		assert.Equal(t, "https://pr-42.preview.diverge.dev", resp.PreviewURL)
	}
}

func TestLLMMeterHandler_RejectsSuspendedOrTerminalTask(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-completed",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Completed task",
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))
	task.Status.Phase = v1alpha1.AgentTaskPhaseCompleted
	require.NoError(t, crdClient.Status().Update(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:    "task-completed",
		Namespace: "default",
		ExpiresAt: time.Now().Add(time.Hour),
	})

	body, _ := json.Marshal(LLMMeterRequest{
		TokensUsed: 100,
		CostUSD:    0.01,
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, req)
	require.Equal(t, http.StatusTooManyRequests, rec.Code)
	var resp LLMMeterResponse
	require.NoError(t, json.NewDecoder(rec.Body).Decode(&resp))
	assert.False(t, resp.Allowed)
	assert.True(t, resp.Suspended)
}

func TestLLMMeterHandler_PRURLReporting(t *testing.T) {
	ctx := context.Background()
	crdClient, verifier, store, handler := setupLLMMeterTest(t)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-pr-report",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:    "Report created PR URL",
			BudgetUSD:    "5.00",
			Capabilities: []string{"claude-sonnet-4*"},
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/divergedev/diverge",
			},
		},
	}
	require.NoError(t, crdClient.Create(ctx, task))
	task.Status.Phase = v1alpha1.AgentTaskPhaseActive
	task.Status.TokensConsumed = 4000
	task.Status.CostUSD = "0.2500"
	require.NoError(t, crdClient.Status().Update(ctx, task))

	token := mintTestMeterToken(t, verifier, store, pkgauth.Claims{
		TaskID:        "task-pr-report",
		Namespace:     "default",
		AllowedModels: []string{"claude-sonnet-4*"},
		MaxCostUSD:    5.00,
		ExpiresAt:     time.Now().Add(time.Hour),
	})

	// 1. Invalid PR URL rejected with 400 Bad Request
	for _, invalidURL := range []string{"ftp://invalid-scheme.example.com/pr/1", "https://:443/pull/42"} {
		badBody, _ := json.Marshal(LLMMeterRequest{
			PRURL: invalidURL,
		})
		badReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(badBody))
		badReq.Header.Set("Authorization", "Bearer "+token)
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, badReq)
		require.Equal(t, http.StatusBadRequest, badRec.Code)
	}

	// 2. Valid zero-cost PR URL report updates status.prURL on AgentTask without altering existing usage
	prBody, _ := json.Marshal(LLMMeterRequest{
		RequestID: "task-pr-report-pr-1",
		Iteration: 1,
		PRURL:     "https://github.com/divergedev/diverge/pull/42",
	})
	prReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(prBody))
	prReq.Header.Set("Authorization", "Bearer "+token)
	prRec := httptest.NewRecorder()
	handler.ServeHTTP(prRec, prReq)
	require.Equal(t, http.StatusOK, prRec.Code)

	var resp LLMMeterResponse
	require.NoError(t, json.NewDecoder(prRec.Body).Decode(&resp))
	assert.True(t, resp.Allowed)
	assert.Equal(t, "https://github.com/divergedev/diverge/pull/42", resp.PRURL)
	assert.Equal(t, int64(4000), resp.TotalTokensUsed)
	assert.InDelta(t, 0.25, resp.TotalCostUSD, 0.0001)

	var updated v1alpha1.AgentTask
	require.NoError(t, crdClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "task-pr-report"}, &updated))
	assert.Equal(t, "https://github.com/divergedev/diverge/pull/42", updated.Status.PRURL)
	assert.Equal(t, int64(4000), updated.Status.TokensConsumed)
	assert.Equal(t, "0.2500", updated.Status.CostUSD)

	// 3. Newer PR URL with distinct RequestID updates Status.PRURL, and replaying older RequestID does not overwrite it
	newerBody, _ := json.Marshal(LLMMeterRequest{
		RequestID: "task-pr-report-pr-2",
		Iteration: 2,
		PRURL:     "https://github.com/divergedev/diverge/pull/43",
	})
	newerReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(newerBody))
	newerReq.Header.Set("Authorization", "Bearer "+token)
	newerRec := httptest.NewRecorder()
	handler.ServeHTTP(newerRec, newerReq)
	require.Equal(t, http.StatusOK, newerRec.Code)

	replayOldReq := httptest.NewRequest(http.MethodPost, "/api/v1/agenttask/meter", bytes.NewReader(prBody))
	replayOldReq.Header.Set("Authorization", "Bearer "+token)
	replayOldRec := httptest.NewRecorder()
	handler.ServeHTTP(replayOldRec, replayOldReq)
	require.Equal(t, http.StatusOK, replayOldRec.Code)

	require.NoError(t, crdClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "task-pr-report"}, &updated))
	assert.Equal(t, "https://github.com/divergedev/diverge/pull/43", updated.Status.PRURL)
}

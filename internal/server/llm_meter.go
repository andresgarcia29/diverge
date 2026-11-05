package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/client-go/util/retry"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/divergedev/diverge/api/v1alpha1"
	pkgauth "github.com/divergedev/diverge/pkg/auth"
)

var (
	errTokensOverflow = errors.New("tokensUsed overflows accumulated usage")
	errTaskNotActive  = errors.New("agent task is paused or in terminal state")
)

// LLMMeterRequest is the JSON payload sent by a sandbox LLM proxy or agent to report token and cost usage.
type LLMMeterRequest struct {
	RequestID  string  `json:"requestId,omitempty"`
	Iteration  int32   `json:"iteration,omitempty"`
	Model      string  `json:"model"`
	TokensUsed int64   `json:"tokensUsed"`
	CostUSD    float64 `json:"costUSD"`
	PRURL      string  `json:"prUrl,omitempty"`
}

// LLMMeterResponse is returned by POST /api/v1/agenttask/meter.
type LLMMeterResponse struct {
	Allowed         bool    `json:"allowed"`
	Suspended       bool    `json:"suspended"`
	Reason          string  `json:"reason,omitempty"`
	TotalTokensUsed int64   `json:"totalTokensUsed"`
	TotalCostUSD    float64 `json:"totalCostUSD"`
	Iteration       int32   `json:"iteration,omitempty"`
	MaxIterations   int32   `json:"maxIterations,omitempty"`
	Guidance        string  `json:"guidance,omitempty"`
	PreviewURL      string  `json:"previewUrl,omitempty"`
	PRURL           string  `json:"prUrl,omitempty"`
}

// LLMMeterHandler enforces live cost, token, and model capability budgets for AgentTasks.
type LLMMeterHandler struct {
	taskLocks        [256]sync.Mutex
	crdClient        client.Client
	macaroonVerifier *pkgauth.MacaroonProvider
	tokenStore       pkgauth.TokenStore
	logger           *slog.Logger
}

func (h *LLMMeterHandler) taskLock(namespace, taskID string) *sync.Mutex {
	hasher := fnv.New64a()
	_, _ = hasher.Write([]byte(namespace + "/" + taskID))
	return &h.taskLocks[hasher.Sum64()%uint64(len(h.taskLocks))]
}

// NewLLMMeterHandler creates a new LLMMeterHandler.
func NewLLMMeterHandler(
	crdClient client.Client,
	macaroonVerifier *pkgauth.MacaroonProvider,
	tokenStore pkgauth.TokenStore,
	logger *slog.Logger,
) *LLMMeterHandler {
	if logger == nil {
		logger = slog.Default()
	}
	return &LLMMeterHandler{
		crdClient:        crdClient,
		macaroonVerifier: macaroonVerifier,
		tokenStore:       tokenStore,
		logger:           logger,
	}
}

// ServeHTTP handles POST /api/v1/agenttask/meter requests.
func (h *LLMMeterHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeMeterJSON(w, http.StatusMethodNotAllowed, LLMMeterResponse{
			Allowed: false,
			Reason:  "method not allowed",
		})
		return
	}

	rawTokenStr := extractMeterBearerToken(r.Header.Get("Authorization"))
	if rawTokenStr == "" {
		writeMeterJSON(w, http.StatusUnauthorized, LLMMeterResponse{
			Allowed: false,
			Reason:  "missing or invalid authorization header",
		})
		return
	}

	rawToken, err := decodeMeterMacaroonToken(rawTokenStr)
	if err != nil {
		writeMeterJSON(w, http.StatusUnauthorized, LLMMeterResponse{
			Allowed: false,
			Reason:  "malformed capability token",
		})
		return
	}

	if h.macaroonVerifier == nil {
		writeMeterJSON(w, http.StatusServiceUnavailable, LLMMeterResponse{
			Allowed: false,
			Reason:  "macaroon verifier not configured",
		})
		return
	}

	ctx := r.Context()
	_, claims, err := h.macaroonVerifier.VerifyAndExtract(ctx, rawToken)
	if err != nil {
		writeMeterJSON(w, http.StatusUnauthorized, LLMMeterResponse{
			Allowed: false,
			Reason:  "invalid or expired capability token",
		})
		return
	}

	ns := claims.Namespace
	if ns == "" {
		ns = "default"
	}

	if revoked, revErr := h.checkTokenRevoked(ctx, claims.TaskID, ns); revErr != nil {
		writeMeterJSON(w, http.StatusServiceUnavailable, LLMMeterResponse{
			Allowed: false,
			Reason:  "failed to verify token revocation status",
		})
		return
	} else if revoked {
		writeMeterJSON(w, http.StatusUnauthorized, LLMMeterResponse{
			Allowed: false,
			Reason:  "token revoked",
		})
		return
	}

	var req LLMMeterRequest
	r.Body = http.MaxBytesReader(w, r.Body, 64*1024)
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeMeterJSON(w, http.StatusBadRequest, LLMMeterResponse{
			Allowed: false,
			Reason:  "invalid JSON request body",
		})
		return
	}

	if req.TokensUsed < 0 || req.CostUSD < 0 || math.IsNaN(req.CostUSD) || math.IsInf(req.CostUSD, 0) {
		writeMeterJSON(w, http.StatusBadRequest, LLMMeterResponse{
			Allowed: false,
			Reason:  "tokensUsed and costUSD must be non-negative",
		})
		return
	}

	trimmedPRURL := strings.TrimSpace(req.PRURL)
	if trimmedPRURL != "" {
		parsedPRURL, parseErr := url.Parse(trimmedPRURL)
		if parseErr != nil || (parsedPRURL.Scheme != "http" && parsedPRURL.Scheme != "https") || parsedPRURL.Hostname() == "" {
			writeMeterJSON(w, http.StatusBadRequest, LLMMeterResponse{
				Allowed: false,
				Reason:  "invalid prUrl: must be a valid http or https URL",
			})
			return
		}
	}

	isZeroCostPRReport := trimmedPRURL != "" && req.TokensUsed == 0 && req.CostUSD == 0 && strings.TrimSpace(req.Model) == ""
	if len(claims.AllowedModels) > 0 && !isZeroCostPRReport {
		if strings.TrimSpace(req.Model) == "" {
			writeMeterJSON(w, http.StatusForbidden, LLMMeterResponse{
				Allowed:   false,
				Suspended: false,
				Reason:    "model is required when token restricts allowed_models",
			})
			return
		}
		modelAllowed := false
		for _, m := range claims.AllowedModels {
			if pkgauth.MatchesAllowedModel(m, req.Model) {
				modelAllowed = true
				break
			}
		}
		if !modelAllowed {
			writeMeterJSON(w, http.StatusForbidden, LLMMeterResponse{
				Allowed:   false,
				Suspended: false,
				Reason:    fmt.Sprintf("model %q is not allowed", req.Model),
			})
			return
		}
	}

	if h.crdClient == nil {
		writeMeterJSON(w, http.StatusServiceUnavailable, LLMMeterResponse{
			Allowed: false,
			Reason:  "crd client not configured",
		})
		return
	}

	mu := h.taskLock(ns, claims.TaskID)
	mu.Lock()
	defer mu.Unlock()

	if revoked, revErr := h.checkTokenRevoked(ctx, claims.TaskID, ns); revErr != nil {
		writeMeterJSON(w, http.StatusServiceUnavailable, LLMMeterResponse{
			Allowed: false,
			Reason:  "failed to verify token revocation status",
		})
		return
	} else if revoked {
		writeMeterJSON(w, http.StatusUnauthorized, LLMMeterResponse{
			Allowed: false,
			Reason:  "token revoked",
		})
		return
	}

	var (
		task           v1alpha1.AgentTask
		totalCost      float64
		totalTokens    int64
		curIteration   int32
		maxIterations  int32
		guidance       string
		previewURL     string
		prURL          string
		budgetExceeded bool
		exceedReason   string
		loadFailed     bool
	)

	err = retry.RetryOnConflict(retry.DefaultRetry, func() error {
		loadFailed = true
		task = v1alpha1.AgentTask{}
		if getErr := h.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: claims.TaskID}, &task); getErr != nil {
			return getErr
		}
		loadFailed = false

		var prevCost float64
		if task.Status.CostUSD != "" {
			if parsed, parseErr := strconv.ParseFloat(task.Status.CostUSD, 64); parseErr == nil && !math.IsNaN(parsed) && !math.IsInf(parsed, 0) {
				prevCost = parsed
			}
		}
		totalCost = prevCost
		totalTokens = task.Status.TokensConsumed
		curIteration = task.Status.Iteration
		maxIterations = task.Spec.MaxIterations
		prURL = task.Status.PRURL
		if task.Annotations != nil {
			guidance = strings.TrimSpace(task.Annotations["divergedev.com/guidance"])
			previewURL = strings.TrimSpace(task.Annotations["divergedev.com/preview-url"])
		}

		if task.Spec.Suspended ||
			task.Status.Phase == v1alpha1.AgentTaskPhasePaused ||
			task.Status.Phase == v1alpha1.AgentTaskPhaseCompleted ||
			task.Status.Phase == v1alpha1.AgentTaskPhaseFailed {
			return errTaskNotActive
		}

		if req.TokensUsed > math.MaxInt64-task.Status.TokensConsumed {
			return errTokensOverflow
		}

		alreadyCounted := false
		if req.RequestID != "" {
			for _, id := range task.Status.LastMeterRequestIDs {
				if id == req.RequestID {
					alreadyCounted = true
					break
				}
			}
		}

		if !alreadyCounted {
			totalCost = prevCost + req.CostUSD
			totalTokens = task.Status.TokensConsumed + req.TokensUsed

			task.Status.TokensConsumed = totalTokens
			task.Status.CostUSD = fmt.Sprintf("%.4f", totalCost)

			if req.Iteration > task.Status.Iteration {
				task.Status.Iteration = req.Iteration
			}

			if trimmedPRURL != "" {
				task.Status.PRURL = trimmedPRURL
				prURL = trimmedPRURL
			}

			if req.RequestID != "" {
				task.Status.LastMeterRequestIDs = append(task.Status.LastMeterRequestIDs, req.RequestID)
				if len(task.Status.LastMeterRequestIDs) > 32 {
					task.Status.LastMeterRequestIDs = task.Status.LastMeterRequestIDs[len(task.Status.LastMeterRequestIDs)-32:]
				}
			}
		}

		curIteration = task.Status.Iteration

		budgetExceeded = false
		hasSpecUSD := false
		if task.Spec.BudgetUSD != "" {
			if specBudgetUSD, parseErr := strconv.ParseFloat(task.Spec.BudgetUSD, 64); parseErr == nil && specBudgetUSD > 0 {
				hasSpecUSD = true
				if totalCost > specBudgetUSD {
					budgetExceeded = true
				}
			}
		}
		if !hasSpecUSD && claims.MaxCostUSD > 0 && totalCost > claims.MaxCostUSD {
			budgetExceeded = true
		}

		if task.Spec.BudgetTokens > 0 {
			if totalTokens > task.Spec.BudgetTokens {
				budgetExceeded = true
			}
		} else if claims.MaxTokens > 0 && totalTokens > claims.MaxTokens {
			budgetExceeded = true
		}
		exceedReason = "budget exceeded"

		if !budgetExceeded && task.Spec.MaxIterations > 0 && task.Status.Iteration > task.Spec.MaxIterations {
			budgetExceeded = true
			exceedReason = "max iterations exceeded"
		}

		if budgetExceeded {
			task.Status.Phase = v1alpha1.AgentTaskPhasePaused
			if exceedReason == "max iterations exceeded" {
				task.Status.Message = "Max iterations exceeded"
			} else {
				task.Status.Message = "Budget exceeded"
			}
		}
		return h.updateTaskStatus(ctx, &task)
	})
	if err != nil {
		if errors.Is(err, errTaskNotActive) {
			reason := strings.TrimSpace(task.Status.Message)
			if reason == "" {
				reason = "task is paused or in terminal state"
			}
			writeMeterJSON(w, http.StatusTooManyRequests, LLMMeterResponse{
				Allowed:         false,
				Suspended:       true,
				Reason:          reason,
				TotalTokensUsed: totalTokens,
				TotalCostUSD:    totalCost,
				Iteration:       curIteration,
				MaxIterations:   maxIterations,
				Guidance:        guidance,
				PreviewURL:      previewURL,
				PRURL:           prURL,
			})
			return
		}
		if errors.Is(err, errTokensOverflow) {
			writeMeterJSON(w, http.StatusBadRequest, LLMMeterResponse{
				Allowed: false,
				Reason:  "tokensUsed overflows accumulated usage",
			})
			return
		}
		if apierrors.IsNotFound(err) {
			writeMeterJSON(w, http.StatusNotFound, LLMMeterResponse{
				Allowed: false,
				Reason:  "agent task not found",
			})
			return
		}
		if loadFailed {
			h.logger.Error("failed to get AgentTask for metering", "task", claims.TaskID, "namespace", ns, "error", err)
			writeMeterJSON(w, http.StatusInternalServerError, LLMMeterResponse{
				Allowed: false,
				Reason:  "failed to load agent task",
			})
			return
		}
		h.logger.Error("failed to update AgentTask status", "task", claims.TaskID, "namespace", ns, "error", err)
		writeMeterJSON(w, http.StatusInternalServerError, LLMMeterResponse{
			Allowed: false,
			Reason:  "failed to update task status",
		})
		return
	}

	if budgetExceeded {
		if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
			var latest v1alpha1.AgentTask
			if err := h.crdClient.Get(ctx, client.ObjectKey{Namespace: ns, Name: claims.TaskID}, &latest); err != nil {
				return err
			}
			latest.Spec.Suspended = true
			return h.crdClient.Update(ctx, &latest)
		}); err != nil {
			h.logger.Error("failed to suspend AgentTask spec on budget exceeded", "task", claims.TaskID, "namespace", ns, "error", err)
			writeMeterJSON(w, http.StatusInternalServerError, LLMMeterResponse{
				Allowed: false,
				Reason:  "failed to suspend task",
			})
			return
		}

		if h.tokenStore != nil {
			if err := h.tokenStore.RevokeToken(ctx, claims.TaskID, ns); err != nil && !errors.Is(err, pkgauth.ErrTokenNotFound) {
				h.logger.Warn("failed to revoke token on budget exceeded", "task", claims.TaskID, "namespace", ns, "error", err)
			}
		}

		writeMeterJSON(w, http.StatusTooManyRequests, LLMMeterResponse{
			Allowed:         false,
			Suspended:       true,
			Reason:          exceedReason,
			TotalTokensUsed: totalTokens,
			TotalCostUSD:    totalCost,
			Iteration:       curIteration,
			MaxIterations:   maxIterations,
			Guidance:        guidance,
			PreviewURL:      previewURL,
			PRURL:           prURL,
		})
		return
	}

	writeMeterJSON(w, http.StatusOK, LLMMeterResponse{
		Allowed:         true,
		Suspended:       false,
		TotalTokensUsed: totalTokens,
		TotalCostUSD:    totalCost,
		Iteration:       curIteration,
		MaxIterations:   maxIterations,
		Guidance:        guidance,
		PreviewURL:      previewURL,
		PRURL:           prURL,
	})
}

func (h *LLMMeterHandler) checkTokenRevoked(ctx context.Context, taskID, namespace string) (bool, error) {
	if h.tokenStore == nil {
		return false, nil
	}
	if checker, ok := h.tokenStore.(interface {
		CheckRevoked(ctx context.Context, taskID, namespace string) (bool, error)
	}); ok {
		return checker.CheckRevoked(ctx, taskID, namespace)
	}
	return h.tokenStore.IsRevoked(ctx, taskID, namespace), nil
}

func (h *LLMMeterHandler) updateTaskStatus(ctx context.Context, task *v1alpha1.AgentTask) error {
	err := h.crdClient.Status().Update(ctx, task)
	if err != nil && apierrors.IsNotFound(err) {
		return h.crdClient.Update(ctx, task)
	}
	return err
}

func extractMeterBearerToken(authHeader string) string {
	parts := strings.SplitN(authHeader, " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return ""
	}
	return strings.TrimSpace(parts[1])
}

func decodeMeterMacaroonToken(token string) ([]byte, error) {
	trimmed := strings.TrimSpace(token)
	if trimmed == "" {
		return nil, pkgauth.ErrMalformedToken
	}
	if strings.HasPrefix(trimmed, "{") {
		return []byte(trimmed), nil
	}
	for _, enc := range []*base64.Encoding{
		base64.RawStdEncoding,
		base64.StdEncoding,
		base64.RawURLEncoding,
		base64.URLEncoding,
	} {
		if decoded, err := enc.DecodeString(trimmed); err == nil && len(decoded) > 0 && decoded[0] == '{' {
			return decoded, nil
		}
	}
	return nil, pkgauth.ErrMalformedToken
}

func writeMeterJSON(w http.ResponseWriter, status int, payload LLMMeterResponse) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(payload)
}

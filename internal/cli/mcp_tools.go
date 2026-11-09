package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"connectrpc.com/connect"
	"github.com/protocgen/proto2mcp/pkg/mcpruntime"

	divergev1alpha1 "github.com/divergedev/diverge/api/gen/diverge/v1alpha1"
	divergev1alpha1connect "github.com/divergedev/diverge/api/gen/diverge/v1alpha1/divergev1alpha1connect"
	"github.com/divergedev/diverge/pkg/doctor"
	"github.com/divergedev/diverge/pkg/loadtest"
)

var waitForReadySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "Name of the environment to wait for"},
		"namespace": {"type": "string", "description": "Kubernetes namespace of the environment"}
	},
	"required": ["name", "namespace"]
}`)

var fetchErrorsSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "Name of the environment to fetch errors from"},
		"namespace": {"type": "string", "description": "Kubernetes namespace of the environment"},
		"lines": {"type": "integer", "description": "Maximum number of error lines to return (default: 50)"}
	},
	"required": ["name", "namespace"]
}`)

var loadtestSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"target_url": {"type": "string", "description": "Target endpoint URL to benchmark"},
		"routing_key": {"type": "string", "description": "Diverge routing key header (x-diverge-routing-key)"},
		"duration_seconds": {"type": "integer", "description": "Duration in seconds (default: 5)"},
		"rps": {"type": "integer", "description": "Requests per second (0 for unthrottled)"},
		"concurrency": {"type": "integer", "description": "Concurrent worker count (default: 5)"},
		"baseline_compare": {"type": "boolean", "description": "Whether to also benchmark baseline without routing header"}
	},
	"required": ["target_url"]
}`)

func errorToolResult(err error) *mcpruntime.CallToolResult {
	data, _ := json.Marshal(map[string]string{"error": err.Error()})
	return &mcpruntime.CallToolResult{
		Content: json.RawMessage(data),
		IsError: true,
	}
}

// registerWaitForReady registers the diverge_wait_for_ready MCP tool handler.
func registerWaitForReady(registry mcpruntime.Registry, client divergev1alpha1connect.EnvironmentServiceClient) {
	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_wait_for_ready",
		Description: "Block until a preview environment reaches Ready or Failed phase. Use this after creating an environment instead of polling status repeatedly. Returns the final environment state. Maximum wait: 5 minutes.",
		InputSchema: waitForReadySchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		timeout := time.After(5 * time.Minute)
		ticker := time.NewTicker(2 * time.Second)
		defer ticker.Stop()

		checkStatus := func() (*mcpruntime.CallToolResult, bool) {
			resp, err := client.GetEnvironment(ctx, connect.NewRequest(&divergev1alpha1.GetEnvironmentRequest{
				Name:      params.Name,
				Namespace: params.Namespace,
			}))
			if err != nil {
				return nil, false // Retry on transient errors
			}

			env := resp.Msg.Environment
			if env == nil {
				return nil, false
			}

			phase := env.Status.Phase
			if phase == "Running" || phase == "Ready" || phase == "Failed" || phase == "Error" {
				result, _ := json.Marshal(map[string]interface{}{
					"name":      env.Name,
					"namespace": env.Namespace,
					"phase":     phase,
				})
				isErr := phase == "Failed" || phase == "Error"
				return &mcpruntime.CallToolResult{
					Content: json.RawMessage(result),
					IsError: isErr,
				}, true
			}
			return nil, false
		}

		if res, ok := checkStatus(); ok {
			return res, nil
		}

		for {
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-timeout:
				return &mcpruntime.CallToolResult{
					Content: json.RawMessage(`{"error": "timeout waiting for environment to become ready"}`),
					IsError: true,
				}, nil
			case <-ticker.C:
				if res, ok := checkStatus(); ok {
					return res, nil
				}
			}
		}
	})
}

// registerFetchErrors registers the diverge_fetch_errors MCP tool handler.
func registerFetchErrors(registry mcpruntime.Registry, client divergev1alpha1connect.EnvironmentServiceClient) {
	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_fetch_errors",
		Description: "Fetch the last error-level log lines from a preview environment. Use this to debug deployment failures instead of streaming all logs. Returns only ERROR and FATAL level entries, truncated to protect context windows.",
		InputSchema: fetchErrorsSchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
			Lines     int    `json:"lines"`
		}
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		if params.Lines <= 0 {
			params.Lines = 50
		}

		logCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()

		stream, err := client.StreamLogs(logCtx, connect.NewRequest(&divergev1alpha1.StreamLogsRequest{
			EnvironmentName: params.Name,
			Namespace:       params.Namespace,
		}))
		if err != nil {
			if connect.CodeOf(err) == connect.CodeUnimplemented {
				result, _ := json.Marshal(map[string]interface{}{
					"environment": params.Name,
					"namespace":   params.Namespace,
					"error_count": 0,
					"lines":       []string{},
					"warning":     "log streaming is not configured or supported by this server",
				})
				return &mcpruntime.CallToolResult{
					Content: json.RawMessage(result),
				}, nil
			}
			return errorToolResult(err), nil
		}

		errorLines := make([]string, 0)
		for stream.Receive() {
			msg := stream.Msg()
			line := msg.Content
			if containsErrorLevel(line) {
				errorLines = append(errorLines, line)
				if len(errorLines) > params.Lines {
					errorLines = errorLines[1:]
				}
			}
		}

		if streamErr := stream.Err(); streamErr != nil {
			if connect.CodeOf(streamErr) == connect.CodeUnimplemented {
				result, _ := json.Marshal(map[string]interface{}{
					"environment": params.Name,
					"namespace":   params.Namespace,
					"error_count": 0,
					"lines":       []string{},
					"warning":     "log streaming is not configured or supported by this server",
				})
				return &mcpruntime.CallToolResult{
					Content: json.RawMessage(result),
				}, nil
			}
			// Only treat deadline exceeded or canceled as normal completion if our local context was terminated.
			// If logCtx is still active, an error code from the server (e.g. server-side timeout/cancellation)
			// represents a failure.
			isLocalTimeout := errors.Is(logCtx.Err(), context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded)
			isLocalCanceled := errors.Is(logCtx.Err(), context.Canceled) || errors.Is(ctx.Err(), context.Canceled)

			isDeadlineErr := errors.Is(streamErr, context.DeadlineExceeded) || connect.CodeOf(streamErr) == connect.CodeDeadlineExceeded
			isCanceledErr := errors.Is(streamErr, context.Canceled) || connect.CodeOf(streamErr) == connect.CodeCanceled

			isNormalCompletion := (isDeadlineErr && isLocalTimeout) || (isCanceledErr && isLocalCanceled)
			if !isNormalCompletion {
				return errorToolResult(streamErr), nil
			}
		}

		result, _ := json.Marshal(map[string]interface{}{
			"environment": params.Name,
			"namespace":   params.Namespace,
			"error_count": len(errorLines),
			"lines":       errorLines,
		})

		return &mcpruntime.CallToolResult{
			Content: json.RawMessage(result),
		}, nil
	})
}

// containsErrorLevel checks if a log line indicates an error, fatal, or panic state.
func containsErrorLevel(line string) bool {
	lowerLine := strings.ToLower(line)
	for _, indicator := range []string{"error", "fatal", "panic", "level=error", "level=fatal"} {
		if strings.Contains(lowerLine, indicator) {
			return true
		}
	}
	return false
}

// validateTargetURL validates that targetURL has http/https scheme, a non-empty host,
// and protects against SSRF (disallowing cloud metadata addresses, link-local unicast/multicast,
// and enforcing DIVERGE_ALLOWED_HOSTS allowlist if configured).
func validateTargetURL(targetURL string) error {
	return loadtest.ValidateTargetURL(targetURL)
}

// registerLoadtest registers the diverge_loadtest MCP tool handler.
func registerLoadtest(registry mcpruntime.Registry) {
	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_loadtest",
		Description: "Run a targeted ephemeral load and benchmark test against a preview environment or endpoint. Measures latency percentiles (p50, p90, p95, p99) and error rates, with optional baseline comparison.",
		InputSchema: loadtestSchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params struct {
			TargetURL       string `json:"target_url"`
			RoutingKey      string `json:"routing_key"`
			DurationSeconds int    `json:"duration_seconds"`
			RPS             int    `json:"rps"`
			Concurrency     int    `json:"concurrency"`
			BaselineCompare bool   `json:"baseline_compare"`
		}
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		if err := validateTargetURL(params.TargetURL); err != nil {
			return errorToolResult(err), nil
		}

		duration := time.Duration(params.DurationSeconds) * time.Second
		if duration <= 0 {
			duration = 5 * time.Second
		}
		concurrency := params.Concurrency
		if concurrency <= 0 {
			concurrency = 5
		}

		cfg := loadtest.Config{
			TargetURL:       params.TargetURL,
			RoutingKey:      params.RoutingKey,
			Duration:        duration,
			RPS:             params.RPS,
			Concurrency:     concurrency,
			BaselineCompare: params.BaselineCompare,
		}

		runner := loadtest.NewRunner()
		res, err := runner.Run(ctx, cfg)
		if err != nil {
			return errorToolResult(err), nil
		}

		data, err := json.Marshal(res)
		if err != nil {
			return nil, fmt.Errorf("failed to marshal load test result: %w", err)
		}

		return &mcpruntime.CallToolResult{
			Content: json.RawMessage(data),
			IsError: !res.Passed,
		}, nil
	})
}

var doctorSchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "Name of the environment to diagnose"},
		"namespace": {"type": "string", "description": "Kubernetes namespace of the environment"}
	},
	"required": ["name", "namespace"]
}`)

// registerDoctor registers the diverge_doctor MCP tool handler.
func registerDoctor(registry mcpruntime.Registry, client divergev1alpha1connect.EnvironmentServiceClient, diagnoser ...*doctor.Diagnoser) {
	var diag *doctor.Diagnoser
	if len(diagnoser) > 0 {
		diag = diagnoser[0]
	}

	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_doctor",
		Description: "Diagnose an environment or workload failure. Evaluates phase, status conditions, and provides root-cause recommendations.",
		InputSchema: doctorSchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params struct {
			Name      string `json:"name"`
			Namespace string `json:"namespace"`
		}
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}

		healthy := true
		var issues []string
		var remedies []string

		if diag != nil {
			report, err := diag.Diagnose(ctx, params.Namespace, params.Name)
			if err != nil {
				return errorToolResult(err), nil
			}
			healthy = report.Healthy
			for _, iss := range report.Issues {
				issues = append(issues, fmt.Sprintf("[%s] %s: %s", iss.Severity, iss.Component, iss.Summary))
				if iss.Remediation != "" {
					remedies = append(remedies, iss.Remediation)
				}
			}
			remedies = append(remedies, report.Suggestions...)
		} else {
			resp, err := client.GetEnvironment(ctx, connect.NewRequest(&divergev1alpha1.GetEnvironmentRequest{
				Name:      params.Name,
				Namespace: params.Namespace,
			}))
			if err != nil {
				return errorToolResult(err), nil
			}

			env := resp.Msg.Environment
			if env != nil && env.Status != nil {
				phase := env.Status.Phase
				if phase == "Failed" || phase == "Error" {
					healthy = false
					issues = append(issues, fmt.Sprintf("Environment phase is %s", phase))
					remedies = append(remedies, "Inspect logs with diverge_fetch_errors or verify container image tags")
				}
			}
		}

		data, _ := json.Marshal(map[string]interface{}{
			"name":      params.Name,
			"namespace": params.Namespace,
			"healthy":   healthy,
			"issues":    issues,
			"remedies":  remedies,
		})

		return &mcpruntime.CallToolResult{
			Content: json.RawMessage(data),
			IsError: !healthy,
		}, nil
	})
}

var verifySchema = json.RawMessage(`{
	"type": "object",
	"properties": {
		"name": {"type": "string", "description": "Name of the preview environment to verify"},
		"namespace": {"type": "string", "description": "Kubernetes namespace of the environment (default: default)"},
		"path": {"type": "string", "description": "HTTP path or URL to probe (default: /)"},
		"expect_status": {"type": "integer", "description": "Expected HTTP status code (default: any 2xx/3xx)"},
		"max_latency_ms": {"type": "integer", "description": "Maximum acceptable response latency in milliseconds"},
		"compare_baseline": {"type": "boolean", "description": "Whether to also probe the baseline without routing headers and verify status parity"}
	},
	"required": ["name"]
}`)

func probeVerifyURL(ctx context.Context, client *http.Client, targetURL, headerKey, headerValue string) (int, time.Duration, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, targetURL, nil)
	if err != nil {
		return 0, 0, err
	}
	if headerKey != "" && headerValue != "" {
		req.Header.Set(headerKey, headerValue)
		req.Header.Set("baggage", "diverge.env="+headerValue)
	}
	start := time.Now()
	resp, err := client.Do(req)
	dur := time.Since(start)
	if err != nil {
		return 0, dur, err
	}
	_ = resp.Body.Close()
	return resp.StatusCode, dur, nil
}

// registerVerify registers the diverge_verify MCP tool handler.
func registerVerify(registry mcpruntime.Registry, client divergev1alpha1connect.EnvironmentServiceClient, diagnoser ...*doctor.Diagnoser) {
	var diag *doctor.Diagnoser
	if len(diagnoser) > 0 {
		diag = diagnoser[0]
	}

	registry.Register(mcpruntime.ToolDefinition{
		Name:        "diverge_verify",
		Description: "Run structured verification checks (workload diagnostics, HTTP status, latency SLO, and optional baseline parity) against a preview environment.",
		InputSchema: verifySchema,
	}, func(ctx context.Context, req mcpruntime.ToolRequest) (*mcpruntime.CallToolResult, error) {
		var params struct {
			Name            string `json:"name"`
			Namespace       string `json:"namespace"`
			Path            string `json:"path"`
			ExpectStatus    int    `json:"expect_status"`
			MaxLatencyMs    int64  `json:"max_latency_ms"`
			CompareBaseline bool   `json:"compare_baseline"`
		}
		if err := json.Unmarshal(req.Arguments, &params); err != nil {
			return nil, fmt.Errorf("invalid arguments: %w", err)
		}
		params.Name = strings.TrimSpace(params.Name)
		if params.Name == "" {
			return errorToolResult(fmt.Errorf("name is required")), nil
		}
		params.Namespace = strings.TrimSpace(params.Namespace)
		if params.Namespace == "" {
			params.Namespace = "default"
		}
		params.Path = strings.TrimSpace(params.Path)
		if params.Path == "" {
			params.Path = "/"
		}

		resp, err := client.GetEnvironment(ctx, connect.NewRequest(&divergev1alpha1.GetEnvironmentRequest{
			Name:      params.Name,
			Namespace: params.Namespace,
		}))
		if err != nil {
			return errorToolResult(err), nil
		}
		env := resp.Msg.GetEnvironment()
		if env == nil {
			return errorToolResult(fmt.Errorf("environment %s/%s not found", params.Namespace, params.Name)), nil
		}

		checks := make([]VerifyCheckResult, 0, 4)

		if diag != nil {
			report, diagErr := diag.Diagnose(ctx, params.Namespace, params.Name)
			if diagErr != nil {
				checks = append(checks, VerifyCheckResult{
					Name:    "workload-diagnostics",
					Passed:  false,
					Status:  "ERROR",
					Message: diagErr.Error(),
				})
			} else {
				docStatus := "HEALTHY"
				docMsg := "All preview pods, migrations, and routes healthy"
				if !report.Healthy {
					docStatus = "UNHEALTHY"
					docMsg = fmt.Sprintf("%d issue(s) detected", len(report.Issues))
				}
				checks = append(checks, VerifyCheckResult{
					Name:    "workload-diagnostics",
					Passed:  report.Healthy,
					Status:  docStatus,
					Message: docMsg,
				})
			}
		} else {
			phase := env.GetStatus().GetPhase()
			if phase == "" {
				phase = "Unknown"
			}
			healthy := phase != "Failed" && phase != "Error" && phase != "Terminating"
			checks = append(checks, VerifyCheckResult{
				Name:    "workload-diagnostics",
				Passed:  healthy,
				Status:  phase,
				Message: fmt.Sprintf("Environment phase is %s", phase),
			})
		}

		baseURL := strings.TrimSpace(env.GetStatus().GetUrl())
		if baseURL == "" {
			baseURL = strings.TrimSpace(env.GetSpec().GetRouting().GetExternalUrl())
		}
		headerKey := strings.TrimSpace(env.GetSpec().GetRouting().GetHeaderKey())
		if headerKey == "" {
			headerKey = "x-diverge-env"
		}
		headerValue := strings.TrimSpace(env.GetSpec().GetRouting().GetHeaderValue())
		if headerValue == "" {
			headerValue = params.Name
		}

		targetURL, urlErr := buildCurlTargetURL(baseURL, params.Path)
		if urlErr != nil {
			checks = append(checks, VerifyCheckResult{
				Name:    "http-status:" + params.Path,
				Passed:  false,
				Status:  "ERROR",
				Message: urlErr.Error(),
			})
		} else {
			if err := validateTargetURL(targetURL); err != nil {
				return errorToolResult(err), nil
			}
			httpClient := &http.Client{Timeout: 15 * time.Second}
			statusCode, dur, probeErr := probeVerifyURL(ctx, httpClient, targetURL, headerKey, headerValue)
			if probeErr != nil {
				checks = append(checks, VerifyCheckResult{
					Name:    "http-status:" + params.Path,
					Passed:  false,
					Status:  "ERROR",
					Message: probeErr.Error(),
				})
			} else {
				var statusPassed bool
				var statusMsg string
				if params.ExpectStatus > 0 {
					statusPassed = statusCode == params.ExpectStatus
					statusMsg = fmt.Sprintf("expected HTTP %d, got %d (%s)", params.ExpectStatus, statusCode, targetURL)
				} else {
					statusPassed = statusCode >= 200 && statusCode < 400
					statusMsg = fmt.Sprintf("expected 2xx/3xx, got %d (%s)", statusCode, targetURL)
				}
				checks = append(checks, VerifyCheckResult{
					Name:       "http-status:" + params.Path,
					Passed:     statusPassed,
					Status:     fmt.Sprintf("HTTP %d", statusCode),
					DurationMs: dur.Milliseconds(),
					Message:    statusMsg,
				})

				if params.MaxLatencyMs > 0 {
					checks = append(checks, VerifyCheckResult{
						Name:       "latency-slo:" + params.Path,
						Passed:     dur.Milliseconds() <= params.MaxLatencyMs,
						Status:     dur.Round(time.Millisecond).String(),
						DurationMs: dur.Milliseconds(),
						Message:    fmt.Sprintf("threshold <= %dms", params.MaxLatencyMs),
					})
				}

				if params.CompareBaseline {
					baseCode, baseDur, baseErr := probeVerifyURL(ctx, httpClient, targetURL, "", "")
					if baseErr != nil {
						checks = append(checks, VerifyCheckResult{
							Name:    "baseline-parity:" + params.Path,
							Passed:  false,
							Status:  "ERROR",
							Message: fmt.Sprintf("baseline request failed: %v", baseErr),
						})
					} else {
						parity := baseCode == statusCode
						parityStatus := "PARITY"
						if !parity {
							parityStatus = "DIFF"
						}
						checks = append(checks, VerifyCheckResult{
							Name:       "baseline-parity:" + params.Path,
							Passed:     parity,
							Status:     parityStatus,
							DurationMs: baseDur.Milliseconds(),
							Message:    fmt.Sprintf("baseline HTTP %d (%s) vs preview HTTP %d (%s)", baseCode, baseDur.Round(time.Millisecond), statusCode, dur.Round(time.Millisecond)),
						})
					}
				}
			}
		}

		passed := len(checks) > 0
		for _, chk := range checks {
			if !chk.Passed {
				passed = false
				break
			}
		}

		outURL := targetURL
		if outURL == "" {
			outURL = baseURL
		}
		data, _ := json.Marshal(map[string]any{
			"environment": params.Name,
			"namespace":   params.Namespace,
			"url":         outURL,
			"targetUrl":   outURL,
			"header":      fmt.Sprintf("%s: %s", headerKey, headerValue),
			"passed":      passed,
			"checks":      checks,
		})

		return &mcpruntime.CallToolResult{
			Content: json.RawMessage(data),
			IsError: !passed,
		}, nil
	})
}

func customTools() []mcpruntime.ToolDefinition {
	return []mcpruntime.ToolDefinition{
		{
			Name:        "diverge_wait_for_ready",
			Description: "Block until a preview environment reaches Ready or Failed phase. Use this after creating an environment instead of polling status repeatedly. Returns the final environment state. Maximum wait: 5 minutes.",
			InputSchema: waitForReadySchema,
		},
		{
			Name:        "diverge_fetch_errors",
			Description: "Fetch the last error-level log lines from a preview environment. Use this to debug deployment failures instead of streaming all logs. Returns only ERROR and FATAL level entries, truncated to protect context windows.",
			InputSchema: fetchErrorsSchema,
		},
		{
			Name:        "diverge_loadtest",
			Description: "Run a targeted ephemeral load and benchmark test against a preview environment or endpoint. Measures latency percentiles (p50, p90, p95, p99) and error rates, with optional baseline comparison.",
			InputSchema: loadtestSchema,
		},
		{
			Name:        "diverge_doctor",
			Description: "Diagnose an environment or workload failure. Evaluates phase, status conditions, and provides root-cause recommendations.",
			InputSchema: doctorSchema,
		},
		{
			Name:        "diverge_verify",
			Description: "Run structured verification checks (workload diagnostics, HTTP status, latency SLO, and optional baseline parity) against a preview environment.",
			InputSchema: verifySchema,
		},
		{
			Name:        "diverge_visual_test",
			Description: "Run pixel-by-pixel visual regression comparison between baseline and preview screenshots (single file pair or directory batch). Highlights visual drift and optionally writes diff PNG artifacts.",
			InputSchema: visualTestSchema,
		},
		{
			Name:        "diverge_replay",
			Description: "Replay recorded HTTP, ConnectRPC, and Protobuf/gRPC traffic (.jsonl, .har, .json, or inline requests) against baseline and a preview environment. Compares status code parity, p50/p95 latency, JSON structural schemas, and binary Protobuf wire tags/descriptors. Executes requests against baseline and attempts preview only when baseline execution completes without error (including passing the 10 MiB response-size check, whereas baseline HTTP or gRPC status does not determine whether preview is attempted). Automatically strips recorded diverge_env sticky-routing cookies, diverge.env baggage items, and routing headers from baseline requests; supports filter_hosts to retain only matching hostnames, strip_auth to remove Authorization/Proxy-Authorization/Cookie/X-Api-Key headers and URL userinfo, and strip_headers to remove custom headers before replay.",
			InputSchema: replaySchema,
		},
	}
}

func registerCustomTools(registry mcpruntime.Registry, envClient divergev1alpha1connect.EnvironmentServiceClient, diagnoser ...*doctor.Diagnoser) {
	registerWaitForReady(registry, envClient)
	registerFetchErrors(registry, envClient)
	registerLoadtest(registry)
	registerDoctor(registry, envClient, diagnoser...)
	registerVerify(registry, envClient, diagnoser...)
	registerVisualTest(registry)
	registerReplay(registry, envClient)
}

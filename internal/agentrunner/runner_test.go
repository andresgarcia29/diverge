package agentrunner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

type recordedCommand struct {
	Dir  string
	Env  []string
	Name string
	Args []string
}

type mockCommandExecutor struct {
	mu      sync.Mutex
	calls   []recordedCommand
	handler func(callIndex int, cmd recordedCommand) (string, error)
}

func (m *mockCommandExecutor) Run(_ context.Context, dir string, env []string, name string, args ...string) (string, error) {
	m.mu.Lock()
	cmd := recordedCommand{
		Dir:  dir,
		Env:  append([]string(nil), env...),
		Name: name,
		Args: append([]string(nil), args...),
	}
	idx := len(m.calls)
	m.calls = append(m.calls, cmd)
	handler := m.handler
	m.mu.Unlock()

	if handler != nil {
		return handler(idx, cmd)
	}
	return "", nil
}

func (m *mockCommandExecutor) Calls() []recordedCommand {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]recordedCommand, len(m.calls))
	copy(out, m.calls)
	return out
}

func TestLoadConfigFromEnv_DefaultsAndCustomValues(t *testing.T) {
	t.Run("defaults", func(t *testing.T) {
		env := map[string]string{
			"DIVERGE_TASK_NAME": "fix-checkout",
			"DIVERGE_OBJECTIVE": "Fix null pointer in checkout handler",
			"DIVERGE_REPO_URL":  "https://github.com/divergedev/shop.git",
		}
		cfg, err := LoadConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.TaskName != "fix-checkout" {
			t.Errorf("TaskName = %q, want %q", cfg.TaskName, "fix-checkout")
		}
		if cfg.TaskNamespace != DefaultTaskNamespace {
			t.Errorf("TaskNamespace = %q, want %q", cfg.TaskNamespace, DefaultTaskNamespace)
		}
		if cfg.BaseBranch != DefaultBaseBranch {
			t.Errorf("BaseBranch = %q, want %q", cfg.BaseBranch, DefaultBaseBranch)
		}
		if cfg.WorkingBranch != "agent/fix-checkout" {
			t.Errorf("WorkingBranch = %q, want %q", cfg.WorkingBranch, "agent/fix-checkout")
		}
		if !cfg.DraftPR {
			t.Errorf("DraftPR = %v, want true", cfg.DraftPR)
		}
		if cfg.GitHubAPIURL != DefaultGitHubAPIURL {
			t.Errorf("GitHubAPIURL = %q, want %q", cfg.GitHubAPIURL, DefaultGitHubAPIURL)
		}
		if cfg.GitLabAPIURL != DefaultGitLabAPIURL {
			t.Errorf("GitLabAPIURL = %q, want %q", cfg.GitLabAPIURL, DefaultGitLabAPIURL)
		}
		if cfg.MaxIterations != DefaultMaxIterations {
			t.Errorf("MaxIterations = %d, want %d", cfg.MaxIterations, DefaultMaxIterations)
		}
		if cfg.TokenFile != DefaultTokenFile {
			t.Errorf("TokenFile = %q, want %q", cfg.TokenFile, DefaultTokenFile)
		}
		if cfg.MCPURL != DefaultMCPURL {
			t.Errorf("MCPURL = %q, want %q", cfg.MCPURL, DefaultMCPURL)
		}
		if cfg.MeterURL != DefaultMeterURL {
			t.Errorf("MeterURL = %q, want %q", cfg.MeterURL, DefaultMeterURL)
		}
		if cfg.PreviewHeaderKey != DefaultPreviewHeaderKey {
			t.Errorf("PreviewHeaderKey = %q, want %q", cfg.PreviewHeaderKey, DefaultPreviewHeaderKey)
		}
		if cfg.WorkspaceDir != DefaultWorkspaceDir {
			t.Errorf("WorkspaceDir = %q, want %q", cfg.WorkspaceDir, DefaultWorkspaceDir)
		}
		if cfg.Model != DefaultModel {
			t.Errorf("Model = %q, want %q", cfg.Model, DefaultModel)
		}
		if cfg.TokensPerIteration != DefaultTokensPerIteration {
			t.Errorf("TokensPerIteration = %d, want %d", cfg.TokensPerIteration, DefaultTokensPerIteration)
		}
		if cfg.CostPerIterationUSD != DefaultCostPerIterationUSD {
			t.Errorf("CostPerIterationUSD = %v, want %v", cfg.CostPerIterationUSD, DefaultCostPerIterationUSD)
		}
	})

	t.Run("custom values", func(t *testing.T) {
		env := map[string]string{
			"DIVERGE_TASK_NAME":              "fix-auth",
			"DIVERGE_TASK_NAMESPACE":         "staging",
			"DIVERGE_OBJECTIVE":              "Harden JWT validation",
			"DIVERGE_REPO_URL":               "https://github.com/divergedev/auth.git",
			"DIVERGE_BASE_BRANCH":            "develop",
			"DIVERGE_WORKING_BRANCH":         "feat/jwt-hardening",
			"DIVERGE_BUDGET_USD":             "5.00",
			"DIVERGE_MAX_ITERATIONS":         "8",
			"DIVERGE_TOKEN_FILE":             "/tmp/custom-token",
			"DIVERGE_MCP_URL":                "http://localhost:8080/mcp",
			"DIVERGE_METER_URL":              "http://localhost:8080/api/v1/agenttask/meter",
			"DIVERGE_PREVIEW_URL":            "https://pr-42.preview.example.com/healthz",
			"DIVERGE_PREVIEW_HEADER_KEY":     "x-custom-env",
			"DIVERGE_PREVIEW_HEADER_VALUE":   "pr-42",
			"DIVERGE_WORKSPACE_DIR":          "/tmp/ws",
			"DIVERGE_AGENT_CMD":              "claude-code --fix",
			"DIVERGE_VERIFY_CMD":             "go test ./...",
			"DIVERGE_MODEL":                  "claude-3-5-sonnet",
			"DIVERGE_TOKENS_PER_ITERATION":   "4000",
			"DIVERGE_COST_PER_ITERATION_USD": "0.12",
			"DIVERGE_GUIDANCE":               "Avoid touching database migrations",
		}
		cfg, err := LoadConfigFromEnv(func(k string) string { return env[k] })
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if cfg.TaskNamespace != "staging" ||
			cfg.BaseBranch != "develop" ||
			cfg.WorkingBranch != "feat/jwt-hardening" ||
			cfg.BudgetUSD != "5.00" ||
			cfg.MaxIterations != 8 ||
			cfg.TokenFile != "/tmp/custom-token" ||
			cfg.PreviewURL != "https://pr-42.preview.example.com/healthz" ||
			cfg.PreviewHeaderKey != "x-custom-env" ||
			cfg.PreviewHeaderValue != "pr-42" ||
			cfg.WorkspaceDir != "/tmp/ws" ||
			cfg.AgentCommand != "claude-code --fix" ||
			cfg.VerifyCommand != "go test ./..." ||
			cfg.Model != "claude-3-5-sonnet" ||
			cfg.TokensPerIteration != 4000 ||
			cfg.CostPerIterationUSD != 0.12 ||
			cfg.Guidance != "Avoid touching database migrations" {
			t.Fatalf("unexpected custom config: %+v", cfg)
		}
	})

	t.Run("missing required fields", func(t *testing.T) {
		cases := []struct {
			name string
			env  map[string]string
		}{
			{
				name: "missing task name",
				env: map[string]string{
					"DIVERGE_OBJECTIVE": "fix bug",
					"DIVERGE_REPO_URL":  "https://github.com/org/repo.git",
				},
			},
			{
				name: "missing objective",
				env: map[string]string{
					"DIVERGE_TASK_NAME": "task-1",
					"DIVERGE_REPO_URL":  "https://github.com/org/repo.git",
				},
			},
			{
				name: "missing repo url",
				env: map[string]string{
					"DIVERGE_TASK_NAME": "task-1",
					"DIVERGE_OBJECTIVE": "fix bug",
				},
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				if _, err := LoadConfigFromEnv(func(k string) string { return tc.env[k] }); err == nil {
					t.Fatalf("expected error for %s, got nil", tc.name)
				}
			})
		}
	})
}

func writeTempToken(t *testing.T, token string) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "token")
	if err := os.WriteFile(p, []byte(token+"\n"), 0600); err != nil {
		t.Fatalf("write temp token: %v", err)
	}
	return p
}

func TestRunner_Run_HappyPathIteration1(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "")
	tokenPath := writeTempToken(t, "macaroon-secret-token-123")

	var (
		gotAuthHeader string
		gotMeterReq   MeterRequest
		previewHits   int
		gotPreviewEnv string
	)

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuthHeader = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&gotMeterReq); err != nil {
			t.Errorf("decode meter req: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         true,
			Suspended:       false,
			TotalTokensUsed: 2500,
			TotalCostUSD:    0.05,
		})
	}))
	defer meterSrv.Close()

	previewSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		previewHits++
		gotPreviewEnv = r.Header.Get("x-diverge-env")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer previewSrv.Close()

	mockExec := &mockCommandExecutor{
		handler: func(_ int, cmd recordedCommand) (string, error) {
			if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "status" {
				return " M pkg/checkout/handler.go\n", nil
			}
			return "", nil
		},
	}

	cfg := &Config{
		TaskName:            "fix-checkout",
		TaskNamespace:       "default",
		Objective:           "Fix nil pointer in checkout",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/fix-checkout",
		MaxIterations:       3,
		TokenFile:           tokenPath,
		MCPURL:              "http://diverge-server.diverge-system.svc:8080/mcp",
		MeterURL:            meterSrv.URL,
		PreviewURL:          previewSrv.URL,
		PreviewHeaderKey:    "x-diverge-env",
		PreviewHeaderValue:  "pr-fix-checkout",
		WorkspaceDir:        "/workspace/shop",
		AgentCommand:        "run-agent-step",
		VerifyCommand:       "go test ./...",
		Model:               "claude-3-7-sonnet",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	var logBuf bytes.Buffer
	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &logBuf,
	}

	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Runner.Run returned unexpected error: %v", err)
	}
	if !res.Succeeded || res.Iterations != 1 || !res.Committed {
		t.Fatalf("unexpected RunResult: %+v", res)
	}
	if res.WorkingBranch != "agent/fix-checkout" || res.TotalTokensUsed != 2500 || res.TotalCostUSD != 0.05 {
		t.Fatalf("unexpected RunResult metrics: %+v", res)
	}

	if gotAuthHeader != "Bearer macaroon-secret-token-123" {
		t.Errorf("Authorization header = %q, want %q", gotAuthHeader, "Bearer macaroon-secret-token-123")
	}
	if gotMeterReq.Model != "claude-3-7-sonnet" || gotMeterReq.TokensUsed != 2500 || gotMeterReq.CostUSD != 0.05 {
		t.Errorf("unexpected MeterRequest: %+v", gotMeterReq)
	}
	if previewHits != 1 || gotPreviewEnv != "pr-fix-checkout" {
		t.Errorf("previewHits = %d, gotPreviewEnv = %q", previewHits, gotPreviewEnv)
	}

	calls := mockExec.Calls()
	if len(calls) != 8 {
		t.Fatalf("expected 8 command executions, got %d: %+v", len(calls), calls)
	}

	// 0: git clone
	if calls[0].Name != "git" || strings.Join(calls[0].Args, " ") != "clone --depth=50 --branch main https://github.com/divergedev/shop.git /workspace/shop" {
		t.Errorf("unexpected clone call: %+v", calls[0])
	}
	// 1: git checkout -B
	if calls[1].Name != "git" || calls[1].Dir != "/workspace/shop" || strings.Join(calls[1].Args, " ") != "checkout -B agent/fix-checkout" {
		t.Errorf("unexpected checkout call: %+v", calls[1])
	}
	// 2: agent command
	if calls[2].Name != "sh" || strings.Join(calls[2].Args, " ") != "-c run-agent-step" {
		t.Errorf("unexpected agent call: %+v", calls[2])
	}
	// 3: verify command
	if calls[3].Name != "sh" || strings.Join(calls[3].Args, " ") != "-c go test ./..." {
		t.Errorf("unexpected verify call: %+v", calls[3])
	}
	// 4: git status --porcelain
	if calls[4].Name != "git" || strings.Join(calls[4].Args, " ") != "status --porcelain" {
		t.Errorf("unexpected status call: %+v", calls[4])
	}
	// 5: git add -A
	if calls[5].Name != "git" || strings.Join(calls[5].Args, " ") != "add -A" {
		t.Errorf("unexpected add call: %+v", calls[5])
	}
	// 6: git commit -m
	if calls[6].Name != "git" || strings.Join(calls[6].Args, " ") != "commit -m fix(fix-checkout): Fix nil pointer in checkout" {
		t.Errorf("unexpected commit call: %+v", calls[6])
	}
	// 7: git push -u origin agent/fix-checkout
	if calls[7].Name != "git" || strings.Join(calls[7].Args, " ") != "push -u origin agent/fix-checkout" {
		t.Errorf("unexpected push call: %+v", calls[7])
	}
}

func TestRunner_Run_MultiIterationRecovery(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "inline-macaroon-token")

	var meterCalls int
	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meterCalls++
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         true,
			Suspended:       false,
			TotalTokensUsed: int64(meterCalls) * 2500,
			TotalCostUSD:    float64(meterCalls) * 0.05,
		})
	}))
	defer meterSrv.Close()

	var verifyAttempts int
	mockExec := &mockCommandExecutor{
		handler: func(_ int, cmd recordedCommand) (string, error) {
			if cmd.Name == "sh" && len(cmd.Args) == 2 && cmd.Args[1] == "go test ./..." {
				verifyAttempts++
				if verifyAttempts == 1 {
					return "FAIL: TestCheckout", errors.New("exit status 1")
				}
				return "PASS", nil
			}
			if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "status" {
				return " M handler.go\n", nil
			}
			return "", nil
		},
	}

	cfg := &Config{
		TaskName:            "fix-retry",
		TaskNamespace:       "default",
		Objective:           "Retry until tests pass",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/fix-retry",
		MaxIterations:       4,
		MeterURL:            meterSrv.URL,
		WorkspaceDir:        "/workspace/shop",
		AgentCommand:        "apply-patch",
		VerifyCommand:       "go test ./...",
		Model:               "claude-3-7-sonnet",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Succeeded || res.Iterations != 2 || !res.Committed {
		t.Fatalf("expected success on iteration 2, got: %+v", res)
	}
	if meterCalls != 2 || verifyAttempts != 2 {
		t.Fatalf("meterCalls = %d, verifyAttempts = %d; want 2 each", meterCalls, verifyAttempts)
	}
	if res.TotalTokensUsed != 5000 || res.TotalCostUSD != 0.10 {
		t.Fatalf("unexpected accumulated usage: %+v", res)
	}
}

func TestRunner_Run_HaltsOnBudgetExceeded(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "inline-macaroon-token")

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         false,
			Suspended:       true,
			Reason:          "budget exceeded",
			TotalTokensUsed: 12500,
			TotalCostUSD:    1.05,
		})
	}))
	defer meterSrv.Close()

	mockExec := &mockCommandExecutor{}

	cfg := &Config{
		TaskName:            "fix-budget",
		TaskNamespace:       "default",
		Objective:           "Test budget guardrail",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/fix-budget",
		MaxIterations:       5,
		MeterURL:            meterSrv.URL,
		WorkspaceDir:        "/workspace/shop",
		AgentCommand:        "should-not-run",
		VerifyCommand:       "should-not-run",
		Model:               "claude-3-7-sonnet",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	res, err := runner.Run(context.Background())
	if !errors.Is(err, ErrBudgetExceeded) {
		t.Fatalf("expected ErrBudgetExceeded, got: %v", err)
	}
	if res == nil || res.Succeeded || res.Iterations != 1 {
		t.Fatalf("unexpected result on budget exceeded: %+v", res)
	}

	// Only git clone and git checkout should have executed; agent command must NOT run.
	calls := mockExec.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected only 2 git setup calls before budget halt, got %d: %+v", len(calls), calls)
	}
}

func TestRunner_Run_HaltsOnModelDisallowed(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "inline-macaroon-token")

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:   false,
			Suspended: false,
			Reason:    `model "gpt-5-ultra" is not allowed`,
		})
	}))
	defer meterSrv.Close()

	mockExec := &mockCommandExecutor{}

	cfg := &Config{
		TaskName:            "fix-model",
		TaskNamespace:       "default",
		Objective:           "Test model caveat enforcement",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/fix-model",
		MaxIterations:       5,
		MeterURL:            meterSrv.URL,
		WorkspaceDir:        "/workspace/shop",
		AgentCommand:        "should-not-run",
		VerifyCommand:       "should-not-run",
		Model:               "gpt-5-ultra",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	_, err := runner.Run(context.Background())
	if !errors.Is(err, ErrModelDisallowed) {
		t.Fatalf("expected ErrModelDisallowed, got: %v", err)
	}

	calls := mockExec.Calls()
	if len(calls) != 2 {
		t.Fatalf("expected only 2 git setup calls before model halt, got %d: %+v", len(calls), calls)
	}
}

func TestRunner_CheckPreviewHealth_StatusCodes(t *testing.T) {
	cases := []struct {
		name       string
		statusCode int
		wantErr    bool
	}{
		{name: "200 OK passes", statusCode: http.StatusOK, wantErr: false},
		{name: "302 Found passes", statusCode: http.StatusFound, wantErr: false},
		{name: "401 Unauthorized fails", statusCode: http.StatusUnauthorized, wantErr: true},
		{name: "403 Forbidden fails", statusCode: http.StatusForbidden, wantErr: true},
		{name: "404 Not Found fails", statusCode: http.StatusNotFound, wantErr: true},
		{name: "500 Internal Server Error fails", statusCode: http.StatusInternalServerError, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var gotHeader string
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotHeader = r.Header.Get("x-diverge-env")
				if tc.statusCode == http.StatusFound {
					w.Header().Set("Location", "/login")
				}
				w.WriteHeader(tc.statusCode)
			}))
			defer srv.Close()

			client := srv.Client()
			client.CheckRedirect = func(_ *http.Request, _ []*http.Request) error {
				return http.ErrUseLastResponse
			}

			runner := &Runner{
				Config: &Config{
					PreviewURL:         srv.URL,
					PreviewHeaderKey:   "x-diverge-env",
					PreviewHeaderValue: "pr-99",
				},
				HTTPClient: client,
			}

			err := runner.CheckPreviewHealth(context.Background())
			if tc.wantErr && err == nil {
				t.Fatalf("expected error for HTTP %d, got nil", tc.statusCode)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("unexpected error for HTTP %d: %v", tc.statusCode, err)
			}
			if gotHeader != "pr-99" {
				t.Errorf("got header %q, want %q", gotHeader, "pr-99")
			}
		})
	}
}

func TestRunner_Run_VerifyCommandReceivesPreviewHeaderEnv(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "inline-macaroon-token")

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         true,
			TotalTokensUsed: 2500,
			TotalCostUSD:    0.05,
		})
	}))
	defer meterSrv.Close()

	previewSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer previewSrv.Close()

	mockExec := &mockCommandExecutor{}
	cfg := &Config{
		TaskName:            "verify-env-test",
		TaskNamespace:       "default",
		Objective:           "Verify env vars",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/verify-env-test",
		MaxIterations:       1,
		MeterURL:            meterSrv.URL,
		PreviewURL:          previewSrv.URL,
		PreviewHeaderKey:    "x-custom-env",
		PreviewHeaderValue:  "pr-verify-1",
		WorkspaceDir:        "/workspace/shop",
		VerifyCommand:       "./scripts/verify.sh",
		Model:               "claude-3-7-sonnet",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Succeeded {
		t.Fatalf("expected succeeded result, got %+v", res)
	}

	calls := mockExec.Calls()
	var verifyCall *recordedCommand
	for i := range calls {
		if calls[i].Name == "sh" && len(calls[i].Args) == 2 && calls[i].Args[1] == "./scripts/verify.sh" {
			verifyCall = &calls[i]
			break
		}
	}
	if verifyCall == nil {
		t.Fatalf("expected verify command call, got calls: %+v", calls)
	}

	envJoined := strings.Join(verifyCall.Env, "\n")
	for _, expected := range []string{
		"DIVERGE_PREVIEW_URL=" + previewSrv.URL,
		"DIVERGE_PREVIEW_HEADER_KEY=x-custom-env",
		"DIVERGE_PREVIEW_HEADER_VALUE=pr-verify-1",
	} {
		if !strings.Contains(envJoined, expected) {
			t.Errorf("verify command env %v missing %q", verifyCall.Env, expected)
		}
	}
}

func TestRunner_Run_TokenRotationAndLiveGuidance(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "")
	tokenPath := writeTempToken(t, "token-v1")
	workspaceDir := t.TempDir()

	var (
		meterCalls  int
		authHeaders []string
		meterReqs   []MeterRequest
	)

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		meterCalls++
		authHeaders = append(authHeaders, r.Header.Get("Authorization"))
		var req MeterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode meter req: %v", err)
		}
		meterReqs = append(meterReqs, req)

		guidance := ""
		if meterCalls == 2 {
			guidance = "Switch to mock Redis client"
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         true,
			TotalTokensUsed: int64(meterCalls) * 2500,
			TotalCostUSD:    float64(meterCalls) * 0.05,
			Iteration:       int32(meterCalls),
			MaxIterations:   3,
			Guidance:        guidance,
		})
	}))
	defer meterSrv.Close()

	var verifyAttempts int
	mockExec := &mockCommandExecutor{
		handler: func(_ int, cmd recordedCommand) (string, error) {
			if cmd.Name == "sh" && len(cmd.Args) == 2 && cmd.Args[1] == "go test ./..." {
				verifyAttempts++
				if verifyAttempts == 1 {
					// Simulate token rotation on disk between iteration 1 and iteration 2
					_ = os.WriteFile(tokenPath, []byte("token-v2\n"), 0600)
					return "FAIL", errors.New("exit status 1")
				}
				return "PASS", nil
			}
			return "", nil
		},
	}

	cfg := &Config{
		TaskName:            "rotate-task",
		TaskNamespace:       "default",
		Objective:           "Test live token rotation and guidance",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/rotate-task",
		MaxIterations:       3,
		TokenFile:           tokenPath,
		MeterURL:            meterSrv.URL,
		WorkspaceDir:        workspaceDir,
		AgentCommand:        "run-agent",
		VerifyCommand:       "go test ./...",
		Model:               "claude-sonnet-4-20250514",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !res.Succeeded || res.Iterations != 2 {
		t.Fatalf("expected success on iteration 2, got %+v", res)
	}
	if len(authHeaders) != 2 || authHeaders[0] != "Bearer token-v1" || authHeaders[1] != "Bearer token-v2" {
		t.Fatalf("expected rotated auth headers [Bearer token-v1, Bearer token-v2], got %v", authHeaders)
	}
	if !strings.HasPrefix(meterReqs[0].RequestID, "rotate-task-") || !strings.HasSuffix(meterReqs[0].RequestID, "-iter-1") || len(meterReqs[0].RequestID) != len("rotate-task-")+32+len("-iter-1") || meterReqs[0].Iteration != 1 {
		t.Errorf("unexpected iter 1 meter req: %+v", meterReqs[0])
	}
	expectedIter2ReqID := strings.TrimSuffix(meterReqs[0].RequestID, "-iter-1") + "-iter-2"
	if meterReqs[1].RequestID != expectedIter2ReqID || meterReqs[1].Iteration != 2 {
		t.Errorf("unexpected iter 2 meter req: %+v (want RequestID %q)", meterReqs[1], expectedIter2ReqID)
	}

	// Verify live guidance was written to .diverge-guidance.md and passed in DIVERGE_GUIDANCE on iteration 2
	guidanceData, err := os.ReadFile(filepath.Join(workspaceDir, ".diverge-guidance.md"))
	if err != nil {
		t.Fatalf("expected .diverge-guidance.md to be written: %v", err)
	}
	if strings.TrimSpace(string(guidanceData)) != "Switch to mock Redis client" {
		t.Errorf("unexpected guidance file content: %q", string(guidanceData))
	}
}

func TestRunner_Run_CreatesGitHubDraftPRAndReportsPRURL(t *testing.T) {
	t.Setenv("DIVERGE_MACAROON_TOKEN", "inline-macaroon-token")

	var (
		meterReqs       []MeterRequest
		ghPRBody        map[string]any
		ghAuth          string
		pushExecuted    bool
		pushBeforePRReq bool
		prRequestsCount int
	)

	meterSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req MeterRequest
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			t.Errorf("decode meter req: %v", err)
		}
		meterReqs = append(meterReqs, req)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(MeterResponse{
			Allowed:         true,
			TotalTokensUsed: 2500,
			TotalCostUSD:    0.05,
			PRURL:           req.PRURL,
		})
	}))
	defer meterSrv.Close()

	ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		prRequestsCount++
		pushBeforePRReq = pushExecuted
		if r.Method != http.MethodPost || r.URL.Path != "/repos/divergedev/shop/pulls" {
			t.Errorf("unexpected GitHub request: %s %s", r.Method, r.URL.Path)
		}
		ghAuth = r.Header.Get("Authorization")
		if err := json.NewDecoder(r.Body).Decode(&ghPRBody); err != nil {
			t.Errorf("decode gh pr body: %v", err)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusCreated)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"html_url": "https://github.com/divergedev/shop/pull/42",
		})
	}))
	defer ghSrv.Close()

	mockExec := &mockCommandExecutor{
		handler: func(_ int, cmd recordedCommand) (string, error) {
			if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "status" {
				return " M pkg/checkout/handler.go\n", nil
			}
			if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "push" {
				pushExecuted = true
			}
			return "", nil
		},
	}

	cfg := &Config{
		TaskName:            "fix-checkout",
		TaskNamespace:       "default",
		Objective:           "Fix nil pointer in checkout",
		RepoURL:             "https://github.com/divergedev/shop.git",
		BaseBranch:          "main",
		WorkingBranch:       "agent/fix-checkout",
		DraftPR:             true,
		SCMToken:            "ghs_test_token_123",
		GitHubAPIURL:        ghSrv.URL,
		MaxIterations:       2,
		MeterURL:            meterSrv.URL,
		WorkspaceDir:        "/workspace/shop",
		AgentCommand:        "run-agent-step",
		VerifyCommand:       "go test ./...",
		Model:               "claude-3-7-sonnet",
		TokensPerIteration:  2500,
		CostPerIterationUSD: 0.05,
	}

	runner := &Runner{
		Config:     cfg,
		Exec:       mockExec,
		HTTPClient: meterSrv.Client(),
		Logger:     &bytes.Buffer{},
	}

	res, err := runner.Run(context.Background())
	if err != nil {
		t.Fatalf("Runner.Run returned unexpected error: %v", err)
	}
	if !res.Succeeded || !res.Committed || res.PRURL != "https://github.com/divergedev/shop/pull/42" {
		t.Fatalf("unexpected RunResult: %+v", res)
	}
	if !pushExecuted || !pushBeforePRReq || prRequestsCount != 1 {
		t.Fatalf("expected git push before 1 PR request; pushExecuted=%v pushBeforePRReq=%v prRequestsCount=%d", pushExecuted, pushBeforePRReq, prRequestsCount)
	}
	calls := mockExec.Calls()
	var pushCall *recordedCommand
	for i := range calls {
		if calls[i].Name == "git" && len(calls[i].Args) > 0 && calls[i].Args[0] == "push" {
			pushCall = &calls[i]
			break
		}
	}
	if pushCall == nil || !strings.Contains(strings.Join(pushCall.Env, "\n"), "GIT_CONFIG_KEY_0=http.extraHeader") {
		t.Errorf("expected git push to receive GIT_CONFIG_KEY_0=http.extraHeader, got: %+v", pushCall)
	}
	if ghAuth != "Bearer ghs_test_token_123" {
		t.Errorf("ghAuth = %q, want %q", ghAuth, "Bearer ghs_test_token_123")
	}
	if ghPRBody["draft"] != true || ghPRBody["head"] != "agent/fix-checkout" || ghPRBody["base"] != "main" {
		t.Errorf("unexpected GitHub PR payload: %+v", ghPRBody)
	}
	if len(meterReqs) != 2 {
		t.Fatalf("expected 2 meter requests (iteration + PRURL report), got %d: %+v", len(meterReqs), meterReqs)
	}
	if meterReqs[1].PRURL != "https://github.com/divergedev/shop/pull/42" || meterReqs[1].TokensUsed != 0 || meterReqs[1].CostUSD != 0 {
		t.Errorf("unexpected PRURL meter report: %+v", meterReqs[1])
	}

	t.Run("push failure prevents PR creation", func(t *testing.T) {
		var prHit bool
		failGHSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			prHit = true
			w.WriteHeader(http.StatusCreated)
		}))
		defer failGHSrv.Close()

		failExec := &mockCommandExecutor{
			handler: func(_ int, cmd recordedCommand) (string, error) {
				if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "status" {
					return " M pkg/checkout/handler.go\n", nil
				}
				if cmd.Name == "git" && len(cmd.Args) >= 1 && cmd.Args[0] == "push" {
					return "remote rejected", errors.New("exit status 1")
				}
				return "", nil
			},
		}

		failCfg := *cfg
		failCfg.GitHubAPIURL = failGHSrv.URL
		failRunner := &Runner{
			Config:     &failCfg,
			Exec:       failExec,
			HTTPClient: meterSrv.Client(),
			Logger:     &bytes.Buffer{},
		}

		failRes, failErr := failRunner.Run(context.Background())
		if failErr == nil || !strings.Contains(failErr.Error(), "git push") {
			t.Fatalf("expected git push error, got: %v", failErr)
		}
		if failRes == nil || failRes.Succeeded {
			t.Fatalf("expected failed RunResult, got: %+v", failRes)
		}
		if prHit {
			t.Errorf("expected no PR request when git push fails")
		}
	})
}

func TestRunner_CreatePullRequest_GitLabAndIdempotentGitHub422(t *testing.T) {
	t.Run("gitlab draft merge request", func(t *testing.T) {
		var (
			glPath  string
			glToken string
			glBody  map[string]any
		)
		glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			glPath = r.URL.EscapedPath()
			glToken = r.Header.Get("PRIVATE-TOKEN")
			_ = json.NewDecoder(r.Body).Decode(&glBody)
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"web_url": "https://gitlab.com/divergedev/sub/shop/-/merge_requests/19",
			})
		}))
		defer glSrv.Close()

		runner := &Runner{
			Config: &Config{
				TaskName:      "fix-gl",
				Objective:     "Fix GitLab pipeline",
				RepoURL:       "git@gitlab.com:divergedev/sub/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-gl",
				DraftPR:       true,
				GitLabToken:   "glpat-secret",
				GitLabAPIURL:  glSrv.URL + "/api/v4",
			},
			HTTPClient: glSrv.Client(),
		}

		prURL, err := runner.CreatePullRequest(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prURL != "https://gitlab.com/divergedev/sub/shop/-/merge_requests/19" {
			t.Errorf("prURL = %q", prURL)
		}
		if glPath != "/api/v4/projects/divergedev%2Fsub%2Fshop/merge_requests" {
			t.Errorf("glPath = %q", glPath)
		}
		if glToken != "glpat-secret" {
			t.Errorf("glToken = %q", glToken)
		}
		if glBody["title"] != "Draft: fix(fix-gl): Fix GitLab pipeline" {
			t.Errorf("unexpected title: %v", glBody["title"])
		}
	})

	t.Run("gitlab 409 existing MR fallback", func(t *testing.T) {
		glSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusConflict)
				_, _ = w.Write([]byte(`{"message":["Another open merge request already exists for this source branch: !19"]}`))
				return
			}
			if r.Method == http.MethodGet && r.URL.EscapedPath() == "/api/v4/projects/divergedev%2Fshop/merge_requests" {
				q := r.URL.Query()
				if q.Get("source_branch") != "agent/fix-gl-existing" || q.Get("target_branch") != "main" || q.Get("state") != "opened" {
					t.Errorf("unexpected GitLab MR lookup query: %s", r.URL.RawQuery)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"web_url": "https://gitlab.com/divergedev/shop/-/merge_requests/19"},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer glSrv.Close()

		runner := &Runner{
			Config: &Config{
				TaskName:      "fix-gl-existing",
				Objective:     "Reuse existing GitLab MR",
				RepoURL:       "https://gitlab.com/divergedev/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-gl-existing",
				DraftPR:       true,
				SCMToken:      "glpat-secret",
				GitLabAPIURL:  glSrv.URL + "/api/v4",
			},
			HTTPClient: glSrv.Client(),
		}

		prURL, err := runner.CreatePullRequest(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prURL != "https://gitlab.com/divergedev/shop/-/merge_requests/19" {
			t.Errorf("prURL = %q, want %q", prURL, "https://gitlab.com/divergedev/shop/-/merge_requests/19")
		}
	})

	t.Run("github 422 existing PR fallback", func(t *testing.T) {
		ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Method == http.MethodPost {
				w.WriteHeader(http.StatusUnprocessableEntity)
				_, _ = w.Write([]byte(`{"message":"Validation Failed","errors":[{"message":"A pull request already exists"}]}`))
				return
			}
			if r.Method == http.MethodGet && r.URL.Path == "/repos/divergedev/shop/pulls" {
				q := r.URL.Query()
				if q.Get("head") != "divergedev:agent/fix-existing" || q.Get("base") != "main" || q.Get("state") != "open" {
					t.Errorf("unexpected pulls query: %s", r.URL.RawQuery)
					w.WriteHeader(http.StatusNotFound)
					return
				}
				w.WriteHeader(http.StatusOK)
				_ = json.NewEncoder(w).Encode([]map[string]any{
					{"html_url": "https://github.com/divergedev/shop/pull/88"},
				})
				return
			}
			w.WriteHeader(http.StatusNotFound)
		}))
		defer ghSrv.Close()

		runner := &Runner{
			Config: &Config{
				TaskName:      "fix-existing",
				Objective:     "Reuse existing PR",
				RepoURL:       "https://github.com/divergedev/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-existing",
				DraftPR:       false,
				SCMToken:      "ghs_token",
				GitHubAPIURL:  ghSrv.URL,
			},
			HTTPClient: ghSrv.Client(),
		}

		prURL, err := runner.CreatePullRequest(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prURL != "https://github.com/divergedev/shop/pull/88" {
			t.Errorf("prURL = %q, want %q", prURL, "https://github.com/divergedev/shop/pull/88")
		}
	})

	t.Run("github.com ignores custom GitLabAPIURL and refuses self-hosted without explicit API URL", func(t *testing.T) {
		ghSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(http.StatusCreated)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"html_url": "https://github.com/divergedev/shop/pull/99",
			})
		}))
		defer ghSrv.Close()

		runner := &Runner{
			Config: &Config{
				TaskName:      "fix-gh-routing",
				Objective:     "Always route github.com to GitHub",
				RepoURL:       "https://github.com/divergedev/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-gh-routing",
				DraftPR:       true,
				GitHubToken:   "ghs_token",
				GitLabToken:   "glpat_should_not_be_used",
				GitHubAPIURL:  ghSrv.URL,
				GitLabAPIURL:  "https://gitlab.internal.example.com/api/v4",
			},
			HTTPClient: ghSrv.Client(),
		}

		prURL, err := runner.CreatePullRequest(context.Background())
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if prURL != "https://github.com/divergedev/shop/pull/99" {
			t.Errorf("prURL = %q, want %q", prURL, "https://github.com/divergedev/shop/pull/99")
		}

		selfHostedGH := &Runner{
			Config: &Config{
				TaskName:      "fix-ghe",
				Objective:     "Reject default API URL for self-hosted GitHub",
				RepoURL:       "https://ghe.internal.example.com/divergedev/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-ghe",
				SCMToken:      "ghs_token",
				GitHubAPIURL:  DefaultGitHubAPIURL,
			},
		}
		if _, err := selfHostedGH.CreatePullRequest(context.Background()); err == nil {
			t.Errorf("expected error when sending self-hosted repo SCM token to default GitHub API")
		}

		selfHostedGL := &Runner{
			Config: &Config{
				TaskName:      "fix-self-gl",
				Objective:     "Reject default API URL for self-hosted GitLab",
				RepoURL:       "https://gitlab.internal.example.com/divergedev/shop.git",
				BaseBranch:    "main",
				WorkingBranch: "agent/fix-self-gl",
				SCMToken:      "glpat_token",
				GitLabAPIURL:  DefaultGitLabAPIURL,
			},
		}
		if _, err := selfHostedGL.CreatePullRequest(context.Background()); err == nil {
			t.Errorf("expected error when sending self-hosted GitLab repo SCM token to default GitLab API")
		}
	})
}

package agentrunner

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

const (
	DefaultTaskNamespace       = "default"
	DefaultBaseBranch          = "main"
	DefaultMaxIterations       = 5
	DefaultTokenFile           = "/etc/diverge/token/token"
	DefaultMCPURL              = "http://diverge-server.diverge-system.svc:8080/mcp"
	DefaultMeterURL            = "http://diverge-server.diverge-system.svc:8080/api/v1/agenttask/meter"
	DefaultPreviewHeaderKey    = "x-diverge-env"
	DefaultWorkspaceDir        = "/workspace"
	DefaultModel               = "claude-3-7-sonnet"
	DefaultTokensPerIteration  = int64(2500)
	DefaultCostPerIterationUSD = 0.05
	DefaultGitHubAPIURL        = "https://api.github.com"
	DefaultGitLabAPIURL        = "https://gitlab.com/api/v4"
)

var (
	// ErrBudgetExceeded is returned when the LLM metering endpoint reports that the task has exceeded its token or USD budget.
	ErrBudgetExceeded = errors.New("agent task budget exceeded")
	// ErrModelDisallowed is returned when the LLM metering endpoint rejects the requested model under the Macaroon token caveats.
	ErrModelDisallowed = errors.New("agent task model disallowed")
	// ErrMaxIterationsReached is returned when the agent loop exhausts MaxIterations without passing verification.
	ErrMaxIterationsReached = errors.New("max iterations reached without passing verification")
)

// Config holds the runtime configuration for the autonomous agent runner.
type Config struct {
	TaskName            string
	TaskNamespace       string
	Objective           string
	RepoURL             string
	BaseBranch          string
	WorkingBranch       string
	DraftPR             bool
	SCMToken            string
	GitHubToken         string
	GitLabToken         string
	GitHubAPIURL        string
	GitLabAPIURL        string
	BudgetUSD           string
	MaxIterations       int
	TokenFile           string
	MCPURL              string
	MeterURL            string
	PreviewURL          string
	PreviewHeaderKey    string
	PreviewHeaderValue  string
	WorkspaceDir        string
	AgentCommand        string
	VerifyCommand       string
	Guidance            string
	Model               string
	RunID               string
	TokensPerIteration  int64
	CostPerIterationUSD float64
}

// LoadConfigFromEnv parses and validates the agent runner configuration from environment variables.
func LoadConfigFromEnv(getenv func(string) string) (*Config, error) {
	if getenv == nil {
		getenv = os.Getenv
	}

	taskName := strings.TrimSpace(getenv("DIVERGE_TASK_NAME"))
	if taskName == "" {
		return nil, errors.New("DIVERGE_TASK_NAME is required")
	}

	objective := strings.TrimSpace(getenv("DIVERGE_OBJECTIVE"))
	if objective == "" {
		return nil, errors.New("DIVERGE_OBJECTIVE is required")
	}

	repoURL := strings.TrimSpace(getenv("DIVERGE_REPO_URL"))
	if repoURL == "" {
		return nil, errors.New("DIVERGE_REPO_URL is required")
	}

	taskNamespace := strings.TrimSpace(getenv("DIVERGE_TASK_NAMESPACE"))
	if taskNamespace == "" {
		taskNamespace = DefaultTaskNamespace
	}

	baseBranch := strings.TrimSpace(getenv("DIVERGE_BASE_BRANCH"))
	if baseBranch == "" {
		baseBranch = DefaultBaseBranch
	}

	workingBranch := strings.TrimSpace(getenv("DIVERGE_WORKING_BRANCH"))
	if workingBranch == "" {
		workingBranch = fmt.Sprintf("agent/%s", taskName)
	}

	draftPR := true
	if rawDraft := strings.TrimSpace(getenv("DIVERGE_DRAFT_PR")); rawDraft != "" {
		parsed, err := strconv.ParseBool(rawDraft)
		if err != nil {
			return nil, fmt.Errorf("invalid DIVERGE_DRAFT_PR %q: %w", rawDraft, err)
		}
		draftPR = parsed
	}

	scmToken := strings.TrimSpace(getenv("DIVERGE_SCM_TOKEN"))
	githubToken := strings.TrimSpace(getenv("GITHUB_TOKEN"))
	gitlabToken := strings.TrimSpace(getenv("GITLAB_TOKEN"))

	githubAPIURL := strings.TrimSpace(getenv("DIVERGE_GITHUB_API_URL"))
	if githubAPIURL == "" {
		githubAPIURL = strings.TrimSpace(getenv("GITHUB_API_URL"))
	}
	if githubAPIURL == "" {
		githubAPIURL = DefaultGitHubAPIURL
	}

	gitlabAPIURL := strings.TrimSpace(getenv("DIVERGE_GITLAB_API_URL"))
	if gitlabAPIURL == "" {
		gitlabAPIURL = strings.TrimSpace(getenv("CI_API_V4_URL"))
	}
	if gitlabAPIURL == "" {
		gitlabAPIURL = DefaultGitLabAPIURL
	}

	maxIterations := DefaultMaxIterations
	if rawMax := strings.TrimSpace(getenv("DIVERGE_MAX_ITERATIONS")); rawMax != "" {
		parsed, err := strconv.Atoi(rawMax)
		if err != nil {
			return nil, fmt.Errorf("invalid DIVERGE_MAX_ITERATIONS %q: %w", rawMax, err)
		}
		if parsed > 0 {
			maxIterations = parsed
		}
	}

	tokenFile := strings.TrimSpace(getenv("DIVERGE_TOKEN_FILE"))
	if tokenFile == "" {
		tokenFile = DefaultTokenFile
	}

	mcpURL := strings.TrimSpace(getenv("DIVERGE_MCP_URL"))
	if mcpURL == "" {
		mcpURL = DefaultMCPURL
	}

	meterURL := strings.TrimSpace(getenv("DIVERGE_METER_URL"))
	if meterURL == "" {
		meterURL = DefaultMeterURL
	}

	previewHeaderKey := strings.TrimSpace(getenv("DIVERGE_PREVIEW_HEADER_KEY"))
	if previewHeaderKey == "" {
		previewHeaderKey = DefaultPreviewHeaderKey
	}

	workspaceDir := strings.TrimSpace(getenv("DIVERGE_WORKSPACE_DIR"))
	if workspaceDir == "" {
		workspaceDir = DefaultWorkspaceDir
	}

	model := strings.TrimSpace(getenv("DIVERGE_MODEL"))
	if model == "" {
		model = DefaultModel
	}

	tokensPerIteration := DefaultTokensPerIteration
	if rawTokens := strings.TrimSpace(getenv("DIVERGE_TOKENS_PER_ITERATION")); rawTokens != "" {
		parsed, err := strconv.ParseInt(rawTokens, 10, 64)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid DIVERGE_TOKENS_PER_ITERATION %q", rawTokens)
		}
		tokensPerIteration = parsed
	}

	costPerIterationUSD := DefaultCostPerIterationUSD
	if rawCost := strings.TrimSpace(getenv("DIVERGE_COST_PER_ITERATION_USD")); rawCost != "" {
		parsed, err := strconv.ParseFloat(rawCost, 64)
		if err != nil || parsed < 0 {
			return nil, fmt.Errorf("invalid DIVERGE_COST_PER_ITERATION_USD %q", rawCost)
		}
		costPerIterationUSD = parsed
	}

	return &Config{
		TaskName:            taskName,
		TaskNamespace:       taskNamespace,
		Objective:           objective,
		RepoURL:             repoURL,
		BaseBranch:          baseBranch,
		WorkingBranch:       workingBranch,
		DraftPR:             draftPR,
		SCMToken:            scmToken,
		GitHubToken:         githubToken,
		GitLabToken:         gitlabToken,
		GitHubAPIURL:        githubAPIURL,
		GitLabAPIURL:        gitlabAPIURL,
		BudgetUSD:           strings.TrimSpace(getenv("DIVERGE_BUDGET_USD")),
		MaxIterations:       maxIterations,
		TokenFile:           tokenFile,
		MCPURL:              mcpURL,
		MeterURL:            meterURL,
		PreviewURL:          strings.TrimSpace(getenv("DIVERGE_PREVIEW_URL")),
		PreviewHeaderKey:    previewHeaderKey,
		PreviewHeaderValue:  strings.TrimSpace(getenv("DIVERGE_PREVIEW_HEADER_VALUE")),
		WorkspaceDir:        workspaceDir,
		AgentCommand:        strings.TrimSpace(getenv("DIVERGE_AGENT_CMD")),
		VerifyCommand:       strings.TrimSpace(getenv("DIVERGE_VERIFY_CMD")),
		Guidance:            strings.TrimSpace(getenv("DIVERGE_GUIDANCE")),
		Model:               model,
		RunID:               strings.TrimSpace(getenv("DIVERGE_RUN_ID")),
		TokensPerIteration:  tokensPerIteration,
		CostPerIterationUSD: costPerIterationUSD,
	}, nil
}

// MeterRequest matches internal/server.LLMMeterRequest JSON schema for POST /api/v1/agenttask/meter.
type MeterRequest struct {
	RequestID  string  `json:"requestId,omitempty"`
	Iteration  int32   `json:"iteration,omitempty"`
	Model      string  `json:"model"`
	TokensUsed int64   `json:"tokensUsed"`
	CostUSD    float64 `json:"costUSD"`
	PRURL      string  `json:"prUrl,omitempty"`
}

// MeterResponse matches internal/server.LLMMeterResponse JSON schema returned by POST /api/v1/agenttask/meter.
type MeterResponse struct {
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

// CommandExecutor abstracts external command execution for testability.
type CommandExecutor interface {
	Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error)
}

// OSCommandExecutor runs commands on the host OS via os/exec.
type OSCommandExecutor struct{}

// Run executes a command with context, working directory, and additional environment variables.
func (e OSCommandExecutor) Run(ctx context.Context, dir string, env []string, name string, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, name, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	if len(env) > 0 {
		cmd.Env = append(os.Environ(), env...)
	}
	out, err := cmd.CombinedOutput()
	if err != nil {
		return string(out), fmt.Errorf("command %s %s failed: %w (output: %s)", name, strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return string(out), nil
}

// RunResult summarizes the outcome of an autonomous agent runner execution.
type RunResult struct {
	Succeeded       bool
	Iterations      int
	Committed       bool
	WorkingBranch   string
	PRURL           string
	TotalTokensUsed int64
	TotalCostUSD    float64
}

// Runner orchestrates the autonomous agent loop inside a Diverge sandbox pod.
type Runner struct {
	Config     *Config
	Exec       CommandExecutor
	HTTPClient *http.Client
	Logger     io.Writer
	runID      string
}

// NewRunner constructs a Runner with default OSCommandExecutor and HTTPClient.
func NewRunner(cfg *Config) *Runner {
	return &Runner{
		Config: cfg,
		Exec:   OSCommandExecutor{},
		HTTPClient: &http.Client{
			Timeout: 30 * time.Second,
		},
		Logger: os.Stdout,
	}
}

// executor returns the configured CommandExecutor or falls back to OSCommandExecutor.
func (r *Runner) executor() CommandExecutor {
	if r.Exec != nil {
		return r.Exec
	}
	return OSCommandExecutor{}
}

// httpClient returns the configured HTTP client or falls back to a 30-second timeout client.
func (r *Runner) httpClient() *http.Client {
	if r.HTTPClient != nil {
		return r.HTTPClient
	}
	return &http.Client{Timeout: 30 * time.Second}
}

// logf formats and writes a line to the runner's logger when configured.
func (r *Runner) logf(format string, args ...any) {
	w := r.Logger
	if w == nil {
		w = io.Discard
	}
	_, _ = fmt.Fprintf(w, format+"\n", args...)
}

// ResolveToken loads the Macaroon capability token from DIVERGE_MACAROON_TOKEN or Config.TokenFile.
func (r *Runner) ResolveToken() (string, error) {
	if envToken := strings.TrimSpace(os.Getenv("DIVERGE_MACAROON_TOKEN")); envToken != "" {
		return envToken, nil
	}
	if r.Config == nil || r.Config.TokenFile == "" {
		return "", errors.New("token file path not configured")
	}
	data, err := os.ReadFile(r.Config.TokenFile)
	if err != nil {
		return "", fmt.Errorf("read capability token from %s: %w", r.Config.TokenFile, err)
	}
	token := strings.TrimSpace(string(data))
	if token == "" {
		return "", fmt.Errorf("capability token file %s is empty", r.Config.TokenFile)
	}
	return token, nil
}

// ReportUsage reports token and cost consumption for an iteration to the Diverge metering endpoint.
func (r *Runner) ReportUsage(ctx context.Context, token, model string, tokensUsed int64, costUSD float64) (*MeterResponse, error) {
	return r.ReportUsageForIteration(ctx, 0, token, model, tokensUsed, costUSD)
}

// ReportUsageForIteration reports token, cost, and iteration consumption with a deterministic idempotency RequestID.
func (r *Runner) ReportUsageForIteration(ctx context.Context, iter int, token, model string, tokensUsed int64, costUSD float64) (*MeterResponse, error) {
	var reqID string
	if iter > 0 && r.Config != nil && r.Config.TaskName != "" {
		runID, err := r.ensureRunID()
		if err != nil {
			return nil, err
		}
		reqID = fmt.Sprintf("%s-%s-iter-%d", r.Config.TaskName, runID, iter)
	}
	return r.postMeterRequest(ctx, token, MeterRequest{
		RequestID:  reqID,
		Iteration:  int32(iter),
		Model:      model,
		TokensUsed: tokensUsed,
		CostUSD:    costUSD,
	})
}

// ReportPullRequest reports a created Pull Request / Merge Request URL to the Diverge metering endpoint.
func (r *Runner) ReportPullRequest(ctx context.Context, iter int, token, prURL string) (*MeterResponse, error) {
	var reqID string
	if r.Config != nil && r.Config.TaskName != "" {
		runID, err := r.ensureRunID()
		if err != nil {
			return nil, err
		}
		sum := sha256.Sum256([]byte(strings.TrimSpace(prURL)))
		reqID = fmt.Sprintf("%s-%s-pr-%s", r.Config.TaskName, runID, hex.EncodeToString(sum[:4]))
	}
	model := ""
	if r.Config != nil {
		model = r.Config.Model
	}
	return r.postMeterRequest(ctx, token, MeterRequest{
		RequestID: reqID,
		Iteration: int32(iter),
		Model:     model,
		PRURL:     prURL,
	})
}

// ensureRunID returns the active run ID or generates a cryptographically random 16-byte hex nonce.
func (r *Runner) ensureRunID() (string, error) {
	if r.runID != "" {
		return r.runID, nil
	}
	if r.Config != nil && r.Config.RunID != "" {
		r.runID = r.Config.RunID
		return r.runID, nil
	}
	nonce := make([]byte, 16)
	if _, err := rand.Read(nonce); err != nil {
		return "", fmt.Errorf("generate run ID: %w", err)
	}
	r.runID = hex.EncodeToString(nonce)
	return r.runID, nil
}

// postMeterRequest serializes and sends a MeterRequest to the Diverge metering endpoint.
func (r *Runner) postMeterRequest(ctx context.Context, token string, payload MeterRequest) (*MeterResponse, error) {
	bodyBytes, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("marshal meter request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, r.Config.MeterURL, bytes.NewReader(bodyBytes))
	if err != nil {
		return nil, fmt.Errorf("create meter request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return nil, fmt.Errorf("post meter usage: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	var meterResp MeterResponse
	if err := json.NewDecoder(resp.Body).Decode(&meterResp); err != nil {
		return nil, fmt.Errorf("decode meter response (status %d): %w", resp.StatusCode, err)
	}

	if resp.StatusCode == http.StatusForbidden {
		return &meterResp, fmt.Errorf("%w: %s", ErrModelDisallowed, meterResp.Reason)
	}

	if resp.StatusCode == http.StatusTooManyRequests || !meterResp.Allowed || meterResp.Suspended {
		return &meterResp, fmt.Errorf("%w: %s", ErrBudgetExceeded, meterResp.Reason)
	}

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return &meterResp, fmt.Errorf("unexpected meter status %d: %s", resp.StatusCode, meterResp.Reason)
	}

	return &meterResp, nil
}

// firstLine extracts and trims the first line of s, capped at 120 bytes.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if idx := strings.IndexAny(s, "\r\n"); idx >= 0 {
		s = strings.TrimSpace(s[:idx])
	}
	if len(s) > 120 {
		return s[:120]
	}
	return s
}

// parseRepositoryCoordinates extracts the SCM hostname and owner/repository path from an HTTPS or SSH Git URL.
func parseRepositoryCoordinates(rawURL string) (host string, repoPath string, err error) {
	trimmed := strings.TrimSpace(rawURL)
	if trimmed == "" {
		return "", "", errors.New("repository URL is empty")
	}

	// Handle SCP-like SSH syntax: git@github.com:owner/repo.git
	if !strings.Contains(trimmed, "://") && strings.Contains(trimmed, ":") {
		parts := strings.SplitN(trimmed, ":", 2)
		hostPart := parts[0]
		if at := strings.LastIndex(hostPart, "@"); at >= 0 {
			hostPart = hostPart[at+1:]
		}
		pathPart := strings.Trim(parts[1], "/")
		pathPart = strings.TrimSuffix(pathPart, ".git")
		if hostPart == "" || !strings.Contains(pathPart, "/") {
			return "", "", fmt.Errorf("cannot extract repository path from %q", rawURL)
		}
		return hostPart, pathPart, nil
	}

	parsed, err := url.Parse(trimmed)
	if err != nil || parsed.Hostname() == "" {
		return "", "", fmt.Errorf("invalid repository URL %q", rawURL)
	}
	pathPart := strings.Trim(parsed.Path, "/")
	pathPart = strings.TrimSuffix(pathPart, ".git")
	if !strings.Contains(pathPart, "/") {
		return "", "", fmt.Errorf("cannot extract owner/repo from %q", rawURL)
	}
	return parsed.Hostname(), pathPart, nil
}

// isGitLabHost determines whether a repository hostname should route to the GitLab API.
// github.com always routes to GitHub even when custom GitLab API environment variables are present.
func (r *Runner) isGitLabHost(host string) bool {
	if strings.EqualFold(host, "github.com") {
		return false
	}
	if strings.Contains(strings.ToLower(host), "gitlab") {
		return true
	}
	if r.Config == nil {
		return false
	}
	gitlabAPI := strings.TrimRight(strings.TrimSpace(r.Config.GitLabAPIURL), "/")
	return gitlabAPI != "" && gitlabAPI != DefaultGitLabAPIURL
}

// resolveProviderSCMToken returns the SCM token matching the target provider (GitLab vs GitHub),
// preferring the explicit DIVERGE_SCM_TOKEN when set.
func (r *Runner) resolveProviderSCMToken(isGitLab bool) string {
	if r.Config == nil {
		return ""
	}
	if scm := strings.TrimSpace(r.Config.SCMToken); scm != "" {
		return scm
	}
	if isGitLab {
		return strings.TrimSpace(r.Config.GitLabToken)
	}
	return strings.TrimSpace(r.Config.GitHubToken)
}

// gitHTTPAuthEnv builds GIT_CONFIG_* environment variables for authenticating HTTPS git push operations
// using the configured provider SCM token without exposing credentials in URLs or command arguments.
func (r *Runner) gitHTTPAuthEnv() []string {
	if r.Config == nil {
		return nil
	}
	repoURL := strings.TrimSpace(r.Config.RepoURL)
	if !strings.HasPrefix(strings.ToLower(repoURL), "https://") {
		return nil
	}
	host, _, err := parseRepositoryCoordinates(repoURL)
	if err != nil {
		return nil
	}
	isGitLab := r.isGitLabHost(host)
	scmToken := r.resolveProviderSCMToken(isGitLab)
	if scmToken == "" {
		return nil
	}
	username := "x-access-token"
	if isGitLab {
		username = "oauth2"
	}
	basic := base64.StdEncoding.EncodeToString([]byte(username + ":" + scmToken))
	return []string{
		"GIT_TERMINAL_PROMPT=0",
		"GIT_CONFIG_COUNT=1",
		"GIT_CONFIG_KEY_0=http.extraHeader",
		"GIT_CONFIG_VALUE_0=Authorization: Basic " + basic,
	}
}

// CreatePullRequest opens a Pull Request (GitHub) or Merge Request (GitLab) for WorkingBranch -> BaseBranch
// when an SCM token is configured for the target provider. If no token is configured, it returns ("", nil).
func (r *Runner) CreatePullRequest(ctx context.Context) (string, error) {
	if r.Config == nil {
		return "", errors.New("runner config is nil")
	}

	host, repoPath, err := parseRepositoryCoordinates(r.Config.RepoURL)
	if err != nil {
		// If no SCM token is configured at all, skip PR creation without failing.
		if strings.TrimSpace(r.Config.SCMToken) == "" && strings.TrimSpace(r.Config.GitHubToken) == "" && strings.TrimSpace(r.Config.GitLabToken) == "" {
			return "", nil
		}
		return "", err
	}

	isGitLab := r.isGitLabHost(host)
	scmToken := r.resolveProviderSCMToken(isGitLab)
	if scmToken == "" {
		return "", nil
	}

	baseBranch := strings.TrimSpace(r.Config.BaseBranch)
	if baseBranch == "" {
		baseBranch = DefaultBaseBranch
	}

	title := fmt.Sprintf("fix(%s): %s", r.Config.TaskName, firstLine(r.Config.Objective))
	body := fmt.Sprintf(
		"Automated pull request created by **Diverge Agent** for task `%s`.\n\n### Objective\n%s\n",
		r.Config.TaskName,
		strings.TrimSpace(r.Config.Objective),
	)

	if isGitLab {
		return r.createGitLabMergeRequest(ctx, scmToken, repoPath, baseBranch, title, body)
	}
	if !strings.EqualFold(host, "github.com") {
		apiBase := strings.TrimRight(strings.TrimSpace(r.Config.GitHubAPIURL), "/")
		if apiBase == "" || apiBase == DefaultGitHubAPIURL {
			return "", fmt.Errorf("repository host %q requires an explicit GitHub API URL; refusing to send SCM token to %s", host, DefaultGitHubAPIURL)
		}
	}
	return r.createGitHubPullRequest(ctx, scmToken, repoPath, baseBranch, title, body)
}

// createGitHubPullRequest creates a GitHub Pull Request or looks up an existing open PR on HTTP 422.
func (r *Runner) createGitHubPullRequest(ctx context.Context, scmToken, repoPath, baseBranch, title, body string) (string, error) {
	apiBase := strings.TrimRight(strings.TrimSpace(r.Config.GitHubAPIURL), "/")
	if apiBase == "" || apiBase == DefaultGitHubAPIURL {
		host, _, err := parseRepositoryCoordinates(r.Config.RepoURL)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(host, "github.com") {
			return "", fmt.Errorf("GitHub API URL must be set explicitly for host %q", host)
		}
		apiBase = DefaultGitHubAPIURL
	}

	payload := map[string]any{
		"title": title,
		"head":  r.Config.WorkingBranch,
		"base":  baseBranch,
		"body":  body,
		"draft": r.Config.DraftPR,
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal GitHub PR request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/repos/%s/pulls", apiBase, repoPath)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return "", fmt.Errorf("create GitHub PR request: %w", err)
	}
	req.Header.Set("Authorization", "Bearer "+scmToken)
	req.Header.Set("Accept", "application/vnd.github+json")
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("post GitHub PR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var prResp struct {
			HTMLURL string `json:"html_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&prResp); err != nil {
			return "", fmt.Errorf("decode GitHub PR response: %w", err)
		}
		return strings.TrimSpace(prResp.HTMLURL), nil
	}

	// If a PR already exists for this branch (422 Unprocessable Entity), look it up idempotently.
	if resp.StatusCode == http.StatusUnprocessableEntity {
		owner := strings.SplitN(repoPath, "/", 2)[0]
		q := url.Values{}
		q.Set("head", fmt.Sprintf("%s:%s", owner, r.Config.WorkingBranch))
		q.Set("base", baseBranch)
		q.Set("state", "open")
		listReq, listErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if listErr == nil {
			listReq.Header.Set("Authorization", "Bearer "+scmToken)
			listReq.Header.Set("Accept", "application/vnd.github+json")
			if listResp, doErr := r.httpClient().Do(listReq); doErr == nil {
				defer func() { _ = listResp.Body.Close() }()
				if listResp.StatusCode == http.StatusOK {
					var existing []struct {
						HTMLURL string `json:"html_url"`
					}
					if json.NewDecoder(listResp.Body).Decode(&existing) == nil && len(existing) > 0 && strings.TrimSpace(existing[0].HTMLURL) != "" {
						return strings.TrimSpace(existing[0].HTMLURL), nil
					}
				}
			}
		}
	}

	errData, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return "", fmt.Errorf("GitHub PR creation returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errData)))
}

// createGitLabMergeRequest creates a GitLab Merge Request or looks up an existing opened MR on HTTP 409/422.
func (r *Runner) createGitLabMergeRequest(ctx context.Context, scmToken, repoPath, baseBranch, title, body string) (string, error) {
	apiBase := strings.TrimRight(strings.TrimSpace(r.Config.GitLabAPIURL), "/")
	if apiBase == "" || apiBase == DefaultGitLabAPIURL {
		host, _, err := parseRepositoryCoordinates(r.Config.RepoURL)
		if err != nil {
			return "", err
		}
		if !strings.EqualFold(host, "gitlab.com") {
			return "", fmt.Errorf("GitLab API URL must be set explicitly for host %q", host)
		}
		apiBase = DefaultGitLabAPIURL
	}

	mrTitle := title
	if r.Config.DraftPR && !strings.HasPrefix(strings.ToLower(mrTitle), "draft:") {
		mrTitle = "Draft: " + mrTitle
	}
	if runes := []rune(mrTitle); len(runes) > 255 {
		mrTitle = string(runes[:255])
	}

	payload := map[string]any{
		"source_branch": r.Config.WorkingBranch,
		"target_branch": baseBranch,
		"title":         mrTitle,
		"description":   body,
	}
	rawBody, err := json.Marshal(payload)
	if err != nil {
		return "", fmt.Errorf("marshal GitLab MR request: %w", err)
	}

	endpoint := fmt.Sprintf("%s/projects/%s/merge_requests", apiBase, url.PathEscape(repoPath))
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(rawBody))
	if err != nil {
		return "", fmt.Errorf("create GitLab MR request: %w", err)
	}
	req.Header.Set("PRIVATE-TOKEN", scmToken)
	req.Header.Set("Authorization", "Bearer "+scmToken)
	req.Header.Set("Content-Type", "application/json")

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return "", fmt.Errorf("post GitLab MR: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode == http.StatusCreated || resp.StatusCode == http.StatusOK {
		var mrResp struct {
			WebURL string `json:"web_url"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&mrResp); err != nil {
			return "", fmt.Errorf("decode GitLab MR response: %w", err)
		}
		return strings.TrimSpace(mrResp.WebURL), nil
	}

	// If an open MR already exists for this source/target branch (409 Conflict or 422 Unprocessable Entity), look it up idempotently.
	if resp.StatusCode == http.StatusConflict || resp.StatusCode == http.StatusUnprocessableEntity {
		q := url.Values{}
		q.Set("source_branch", r.Config.WorkingBranch)
		q.Set("target_branch", baseBranch)
		q.Set("state", "opened")
		listReq, listErr := http.NewRequestWithContext(ctx, http.MethodGet, endpoint+"?"+q.Encode(), nil)
		if listErr == nil {
			listReq.Header.Set("PRIVATE-TOKEN", scmToken)
			listReq.Header.Set("Authorization", "Bearer "+scmToken)
			if listResp, doErr := r.httpClient().Do(listReq); doErr == nil {
				defer func() { _ = listResp.Body.Close() }()
				if listResp.StatusCode == http.StatusOK {
					var existing []struct {
						WebURL string `json:"web_url"`
					}
					if json.NewDecoder(listResp.Body).Decode(&existing) == nil && len(existing) > 0 && strings.TrimSpace(existing[0].WebURL) != "" {
						return strings.TrimSpace(existing[0].WebURL), nil
					}
				}
			}
		}
	}

	errData, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return "", fmt.Errorf("GitLab MR creation returned HTTP %d: %s", resp.StatusCode, strings.TrimSpace(string(errData)))
}

// ensureGitExcludeGuidance appends guidance file patterns to .git/info/exclude when a .git directory exists
// so live steering instructions are never staged by git add -A.
func ensureGitExcludeGuidance(workspaceDir string) {
	gitDir := filepath.Join(workspaceDir, ".git")
	info, err := os.Stat(gitDir)
	if err != nil || !info.IsDir() {
		return
	}
	infoDir := filepath.Join(gitDir, "info")
	_ = os.MkdirAll(infoDir, 0755)
	excludePath := filepath.Join(infoDir, "exclude")
	existing, _ := os.ReadFile(excludePath)
	content := string(existing)
	if !strings.Contains(content, ".diverge-guidance.md") {
		if len(content) > 0 && !strings.HasSuffix(content, "\n") {
			content += "\n"
		}
		content += ".diverge-guidance.md\n.diverge/guidance.md\n"
		_ = os.WriteFile(excludePath, []byte(content), 0600)
	}
}

// CheckPreviewHealth probes the ephemeral preview URL (if configured) with the Diverge isolation header.
func (r *Runner) CheckPreviewHealth(ctx context.Context) error {
	if r.Config == nil || r.Config.PreviewURL == "" {
		return nil
	}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, r.Config.PreviewURL, nil)
	if err != nil {
		return fmt.Errorf("create preview health request: %w", err)
	}
	req.Header.Set("Accept", "application/json, text/html")
	if r.Config.PreviewHeaderKey != "" && r.Config.PreviewHeaderValue != "" {
		req.Header.Set(r.Config.PreviewHeaderKey, r.Config.PreviewHeaderValue)
	}

	resp, err := r.httpClient().Do(req)
	if err != nil {
		return fmt.Errorf("preview health request failed: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("preview health check returned HTTP %d", resp.StatusCode)
}

// Run executes the end-to-end autonomous agent workflow: token resolution, repo clone,
// iterative metered execution + verification, and git commit/push upon passing verification.
func (r *Runner) Run(ctx context.Context) (*RunResult, error) {
	if r.Config == nil {
		return nil, errors.New("runner config is nil")
	}
	r.runID = ""

	token, err := r.ResolveToken()
	if err != nil {
		return nil, err
	}

	exec := r.executor()

	r.logf("cloning repository %s (branch %s) into %s", r.Config.RepoURL, r.Config.BaseBranch, r.Config.WorkspaceDir)
	if _, err := exec.Run(ctx, "", nil, "git", "clone", "--depth=50", "--branch", r.Config.BaseBranch, r.Config.RepoURL, r.Config.WorkspaceDir); err != nil {
		return nil, fmt.Errorf("git clone: %w", err)
	}

	r.logf("checking out working branch %s", r.Config.WorkingBranch)
	if _, err := exec.Run(ctx, r.Config.WorkspaceDir, nil, "git", "checkout", "-B", r.Config.WorkingBranch); err != nil {
		return nil, fmt.Errorf("git checkout working branch: %w", err)
	}

	var (
		lastTokens  int64
		lastCost    float64
		lastFailure string
	)

	for iter := 1; iter <= r.Config.MaxIterations; iter++ {
		r.logf("starting iteration %d/%d", iter, r.Config.MaxIterations)

		if latestToken, tokErr := r.ResolveToken(); tokErr == nil && latestToken != "" {
			token = latestToken
		}

		meterResp, err := r.ReportUsageForIteration(ctx, iter, token, r.Config.Model, r.Config.TokensPerIteration, r.Config.CostPerIterationUSD)
		if meterResp != nil {
			lastTokens = meterResp.TotalTokensUsed
			lastCost = meterResp.TotalCostUSD
			if meterResp.MaxIterations > 0 {
				r.Config.MaxIterations = int(meterResp.MaxIterations)
			}
			if strings.TrimSpace(meterResp.PreviewURL) != "" {
				r.Config.PreviewURL = strings.TrimSpace(meterResp.PreviewURL)
			}
			if liveGuidance := strings.TrimSpace(meterResp.Guidance); liveGuidance != "" {
				r.Config.Guidance = liveGuidance
				if r.Config.WorkspaceDir != "" {
					ensureGitExcludeGuidance(r.Config.WorkspaceDir)
					_ = os.WriteFile(filepath.Join(r.Config.WorkspaceDir, ".diverge-guidance.md"), []byte(liveGuidance+"\n"), 0600)
					divergeDir := filepath.Join(r.Config.WorkspaceDir, ".diverge")
					if mkErr := os.MkdirAll(divergeDir, 0755); mkErr == nil {
						_ = os.WriteFile(filepath.Join(divergeDir, "guidance.md"), []byte(liveGuidance+"\n"), 0600)
					}
				}
			}
		}
		failedResult := func() *RunResult {
			return &RunResult{
				Succeeded:       false,
				Iterations:      iter,
				WorkingBranch:   r.Config.WorkingBranch,
				TotalTokensUsed: lastTokens,
				TotalCostUSD:    lastCost,
			}
		}
		if err != nil {
			return failedResult(), err
		}

		if r.Config.AgentCommand != "" {
			agentEnv := []string{
				fmt.Sprintf("DIVERGE_ITERATION=%d", iter),
				fmt.Sprintf("DIVERGE_OBJECTIVE=%s", r.Config.Objective),
				fmt.Sprintf("DIVERGE_PREVIEW_URL=%s", r.Config.PreviewURL),
				fmt.Sprintf("DIVERGE_PREVIEW_HEADER_KEY=%s", r.Config.PreviewHeaderKey),
				fmt.Sprintf("DIVERGE_PREVIEW_HEADER_VALUE=%s", r.Config.PreviewHeaderValue),
				fmt.Sprintf("DIVERGE_MCP_URL=%s", r.Config.MCPURL),
			}
			if lastFailure != "" {
				agentEnv = append(agentEnv, fmt.Sprintf("DIVERGE_LAST_FAILURE=%s", lastFailure))
			}
			if r.Config.Guidance != "" {
				agentEnv = append(agentEnv, fmt.Sprintf("DIVERGE_GUIDANCE=%s", r.Config.Guidance))
			}
			if _, err := exec.Run(ctx, r.Config.WorkspaceDir, agentEnv, "sh", "-c", r.Config.AgentCommand); err != nil {
				return failedResult(), fmt.Errorf("agent command failed on iteration %d: %w", iter, err)
			}
		}

		if err := r.CheckPreviewHealth(ctx); err != nil {
			lastFailure = err.Error()
			r.logf("iteration %d preview health check failed: %v", iter, err)
			continue
		}

		if r.Config.VerifyCommand != "" {
			verifyEnv := []string{
				fmt.Sprintf("DIVERGE_PREVIEW_URL=%s", r.Config.PreviewURL),
				fmt.Sprintf("DIVERGE_PREVIEW_HEADER_KEY=%s", r.Config.PreviewHeaderKey),
				fmt.Sprintf("DIVERGE_PREVIEW_HEADER_VALUE=%s", r.Config.PreviewHeaderValue),
			}
			if verifyOut, err := exec.Run(ctx, r.Config.WorkspaceDir, verifyEnv, "sh", "-c", r.Config.VerifyCommand); err != nil {
				lastFailure = strings.TrimSpace(verifyOut)
				if lastFailure == "" {
					lastFailure = err.Error()
				}
				r.logf("iteration %d verification failed: %v", iter, err)
				continue
			}
		}

		statusRaw, err := exec.Run(ctx, r.Config.WorkspaceDir, nil, "git", "status", "--porcelain")
		if err != nil {
			return failedResult(), fmt.Errorf("git status: %w", err)
		}

		statusOut := strings.TrimSpace(statusRaw)
		var prURL string
		if statusOut != "" {
			if _, err := exec.Run(ctx, r.Config.WorkspaceDir, nil, "git", "add", "-A"); err != nil {
				return failedResult(), fmt.Errorf("git add: %w", err)
			}
			commitMsg := fmt.Sprintf("fix(%s): %s", r.Config.TaskName, r.Config.Objective)
			if _, err := exec.Run(ctx, r.Config.WorkspaceDir, nil, "git", "commit", "-m", commitMsg); err != nil {
				return failedResult(), fmt.Errorf("git commit: %w", err)
			}
			if _, err := exec.Run(ctx, r.Config.WorkspaceDir, r.gitHTTPAuthEnv(), "git", "push", "-u", "origin", r.Config.WorkingBranch); err != nil {
				return failedResult(), fmt.Errorf("git push: %w", err)
			}

			createdPRURL, prErr := r.CreatePullRequest(ctx)
			if prErr != nil {
				r.logf("warning: failed to create pull request: %v", prErr)
			} else if createdPRURL != "" {
				prURL = createdPRURL
				r.logf("created pull request: %s", prURL)
				reportToken := token
				if latestToken, tokErr := r.ResolveToken(); tokErr == nil && latestToken != "" {
					reportToken = latestToken
				}
				var reportErr error
				for attempt := 1; attempt <= 3; attempt++ {
					if attempt > 1 {
						if latestToken, tokErr := r.ResolveToken(); tokErr == nil && latestToken != "" {
							reportToken = latestToken
						}
					}
					if _, reportErr = r.ReportPullRequest(ctx, iter, reportToken, prURL); reportErr == nil {
						break
					}
				}
				if reportErr != nil {
					r.logf("warning: failed to report pull request URL to metering server: %v", reportErr)
				}
			}
		}

		return &RunResult{
			Succeeded:       true,
			Iterations:      iter,
			Committed:       statusOut != "",
			WorkingBranch:   r.Config.WorkingBranch,
			PRURL:           prURL,
			TotalTokensUsed: lastTokens,
			TotalCostUSD:    lastCost,
		}, nil
	}

	return &RunResult{
		Succeeded:       false,
		Iterations:      r.Config.MaxIterations,
		WorkingBranch:   r.Config.WorkingBranch,
		TotalTokensUsed: lastTokens,
		TotalCostUSD:    lastCost,
	}, ErrMaxIterationsReached
}

package webhook

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

func TestParseSlashCommand(t *testing.T) {
	tests := []struct {
		name    string
		body    string
		wantOK  bool
		wantCmd *SlashCommand
	}{
		{
			name:   "fix command with objective",
			body:   "/diverge fix resolve nil pointer in payment handler",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:    "fix",
				Objective: "resolve nil pointer in payment handler",
			},
		},
		{
			name:   "fix command without objective rejected",
			body:   "/diverge fix   ",
			wantOK: false,
		},
		{
			name:   "preview command",
			body:   "Looks good!\n/diverge preview\nthanks",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "preview",
			},
		},
		{
			name:   "verify command default path",
			body:   "/diverge verify",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "verify",
				Paths:  []string{"/"},
			},
		},
		{
			name:   "verify command explicit path",
			body:   "/diverge verify /healthz",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "verify",
				Paths:  []string{"/healthz"},
			},
		},
		{
			name:   "replay command multiple paths",
			body:   "/diverge replay /api/v1/health /api/v1/orders",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "replay",
				Paths:  []string{"/api/v1/health", "/api/v1/orders"},
			},
		},
		{
			name:   "replay command rejects external url or traversal",
			body:   "/diverge replay https://evil.example.com/pwn",
			wantOK: false,
		},
		{
			name:   "verify command rejects path traversal",
			body:   "/diverge verify /../etc/passwd",
			wantOK: false,
		},
		{
			name:   "verify command rejects percent-encoded path traversal",
			body:   "/diverge verify /%2e%2e/etc/passwd",
			wantOK: false,
		},
		{
			name:   "replay command rejects percent-encoded slash traversal",
			body:   "/diverge replay /api%2f..%2fadmin",
			wantOK: false,
		},
		{
			name:   "teardown command",
			body:   "/diverge teardown",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "teardown",
			},
		},
		{
			name:   "extend-ttl with explicit duration",
			body:   "/diverge extend-ttl 48h",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:   "extend-ttl",
				Duration: 48 * time.Hour,
			},
		},
		{
			name:   "extend-ttl with default duration",
			body:   "/diverge extend-ttl",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:   "extend-ttl",
				Duration: 24 * time.Hour,
			},
		},
		{
			name:   "extend-ttl with invalid duration rejected",
			body:   "/diverge extend-ttl notaduration",
			wantOK: false,
		},
		{
			name:   "pause command",
			body:   "/diverge pause",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "pause",
			},
		},
		{
			name:   "resume command without args",
			body:   "/diverge resume",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "resume",
			},
		},
		{
			name:   "resume command with budget bump",
			body:   "/diverge resume +$5.00",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:    "resume",
				BudgetUSD: "5.00",
			},
		},
		{
			name:   "resume command with budget and tokens",
			body:   "/diverge resume +$10 50000",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:    "resume",
				BudgetUSD: "10.00",
				Tokens:    50000,
			},
		},
		{
			name:   "budget command with amount",
			body:   "/diverge budget +$7.50",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:    "budget",
				BudgetUSD: "7.50",
			},
		},
		{
			name:   "guide command with text",
			body:   "/diverge guide prioritize backward compatibility in auth.go",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action:   "guide",
				Guidance: "prioritize backward compatibility in auth.go",
			},
		},
		{
			name:   "guide command without text rejected",
			body:   "/diverge guide   ",
			wantOK: false,
		},
		{
			name:   "status command",
			body:   "/diverge status",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "status",
			},
		},
		{
			name:   "help command",
			body:   "/diverge help",
			wantOK: true,
			wantCmd: &SlashCommand{
				Action: "help",
			},
		},
		{
			name:   "unrelated comment",
			body:   "LGTM! Ship it.",
			wantOK: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cmd, ok := ParseSlashCommand(tc.body)
			assert.Equal(t, tc.wantOK, ok)
			if tc.wantOK {
				require.NotNil(t, cmd)
				assert.Equal(t, tc.wantCmd.Action, cmd.Action)
				assert.Equal(t, tc.wantCmd.Objective, cmd.Objective)
				assert.Equal(t, tc.wantCmd.Guidance, cmd.Guidance)
				assert.Equal(t, tc.wantCmd.Duration, cmd.Duration)
				assert.Equal(t, tc.wantCmd.BudgetUSD, cmd.BudgetUSD)
				assert.Equal(t, tc.wantCmd.Tokens, cmd.Tokens)
				assert.Equal(t, tc.wantCmd.Paths, cmd.Paths)
			}
		})
	}
}

func TestExecuteSlashCommand_Lifecycle(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	repoURL := "https://github.com/divergedev/demo-payments-api"
	branch := "feat/payments"
	prNumber := 42

	// 1. /diverge fix creates AgentTask
	fixCmd, ok := ParseSlashCommand("/diverge fix fix flaky currency conversion")
	require.True(t, ok)
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", fixCmd, repoURL, branch, prNumber))

	var taskList v1alpha1.AgentTaskList
	require.NoError(t, c.List(ctx, &taskList, client.InNamespace("default")))
	require.Len(t, taskList.Items, 1)
	task := taskList.Items[0]
	assert.Equal(t, "fix flaky currency conversion", task.Spec.Objective)
	assert.Equal(t, repoURL, task.Spec.Repository.URL)
	assert.Equal(t, branch, task.Spec.Repository.BaseBranch)
	assert.Equal(t, "42", task.Annotations["divergedev.com/pr-number"])
	assert.False(t, task.Spec.Suspended)

	// 2. /diverge pause suspends the AgentTask
	pauseCmd, _ := ParseSlashCommand("/diverge pause")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", pauseCmd, repoURL, prNumberStringToBranch(branch), prNumber))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: "default"}, &task))
	assert.True(t, task.Spec.Suspended)

	// 3. /diverge resume unsuspends the AgentTask
	resumeCmd, _ := ParseSlashCommand("/diverge resume")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", resumeCmd, repoURL, branch, prNumber))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: task.Name, Namespace: "default"}, &task))
	assert.False(t, task.Spec.Suspended)

	// 4. /diverge preview creates Environment
	previewCmd, _ := ParseSlashCommand("/diverge preview")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", previewCmd, repoURL, branch, prNumber))

	envName := previewEnvName("pr", "divergedev/demo-payments-api", prNumber)
	var env v1alpha1.Environment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: envName, Namespace: "default"}, &env))
	assert.Equal(t, branch, env.Spec.Source.Branch)
	assert.Equal(t, prNumber, env.Spec.Source.MR)

	// 5. /diverge extend-ttl updates Environment TTL
	extendCmd, _ := ParseSlashCommand("/diverge extend-ttl 72h")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", extendCmd, repoURL, branch, prNumber))
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: envName, Namespace: "default"}, &env))
	require.NotNil(t, env.Spec.Lifecycle.TTL)
	assert.Equal(t, 72*time.Hour, env.Spec.Lifecycle.TTL.Duration)

	// 6. /diverge teardown deletes Environment
	teardownCmd, _ := ParseSlashCommand("/diverge teardown")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", teardownCmd, repoURL, branch, prNumber))
	err := c.Get(ctx, types.NamespacedName{Name: envName, Namespace: "default"}, &env)
	assert.True(t, apierrors.IsNotFound(err))

	// Also verify PreviewGroup extend-ttl & teardown
	pg := &v1alpha1.PreviewGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-pr-42",
			Annotations: map[string]string{
				"divergedev.com/pr-number": "42",
			},
		},
		Spec: v1alpha1.PreviewGroupSpec{
			Source: v1alpha1.EnvironmentSource{
				Project: "divergedev/demo-payments-api",
				MR:      42,
			},
		},
	}
	require.NoError(t, c.Create(ctx, pg))
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", extendCmd, repoURL, branch, prNumber))
	var updatedPG v1alpha1.PreviewGroup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "pg-pr-42"}, &updatedPG))
	require.NotNil(t, updatedPG.Spec.Lifecycle)
	require.NotNil(t, updatedPG.Spec.Lifecycle.TTL)
	assert.Equal(t, 72*time.Hour, updatedPG.Spec.Lifecycle.TTL.Duration)
}

func TestExecuteSlashCommand_RepoIsolation(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	repoA := "https://github.com/divergedev/repo-a"
	repoB := "https://github.com/divergedev/repo-b"
	prNumber := 42

	// Create resources for repoB with the same PR #42
	fixCmd, _ := ParseSlashCommand("/diverge fix fix repo-b issue")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", fixCmd, repoB, "feat/b", prNumber))

	previewCmd, _ := ParseSlashCommand("/diverge preview")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", previewCmd, repoB, "feat/b", prNumber))

	pgB := &v1alpha1.PreviewGroup{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pg-repo-b-42",
			Annotations: map[string]string{
				"divergedev.com/pr-number": "42",
			},
		},
		Spec: v1alpha1.PreviewGroupSpec{
			Source: v1alpha1.EnvironmentSource{
				Project: "divergedev/repo-b",
				MR:      42,
			},
		},
	}
	require.NoError(t, c.Create(ctx, pgB))

	// Run pause and teardown for repoA PR #42 — repoB resources must remain untouched
	pauseCmd, _ := ParseSlashCommand("/diverge pause")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", pauseCmd, repoA, "feat/a", prNumber))

	teardownCmd, _ := ParseSlashCommand("/diverge teardown")
	require.NoError(t, ExecuteSlashCommand(ctx, c, "default", teardownCmd, repoA, "feat/a", prNumber))

	var taskList v1alpha1.AgentTaskList
	require.NoError(t, c.List(ctx, &taskList, client.InNamespace("default")))
	require.Len(t, taskList.Items, 1)
	assert.False(t, taskList.Items[0].Spec.Suspended, "repoB AgentTask must not be suspended by repoA command")

	envBName := previewEnvName("pr", "divergedev/repo-b", prNumber)
	var envB v1alpha1.Environment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: envBName, Namespace: "default"}, &envB), "repoB Environment must not be deleted by repoA teardown")

	var checkPGB v1alpha1.PreviewGroup
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: "pg-repo-b-42"}, &checkPGB), "repoB PreviewGroup must not be deleted by repoA teardown")
}

func prNumberStringToBranch(b string) string { return b }

func TestExecuteSlashCommand_GuideAndBudget(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	repoURL := "https://github.com/divergedev/demo-orders-api"
	branch := "feat/orders"
	prNumber := 99

	// 1. /diverge fix
	fixCmd, ok := ParseSlashCommand("/diverge fix fix checkout timeout")
	require.True(t, ok)
	feedback, err := ExecuteSlashCommandWithFeedback(ctx, c, "default", fixCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Agent task started")
	assert.Contains(t, feedback, "fix checkout timeout")

	// 2. /diverge guide
	guideCmd, ok := ParseSlashCommand("/diverge guide Use exponential backoff for payment gateway retries")
	require.True(t, ok)
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", guideCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Guidance saved")
	assert.Contains(t, feedback, "Use exponential backoff")

	var taskList v1alpha1.AgentTaskList
	require.NoError(t, c.List(ctx, &taskList, client.InNamespace("default")))
	require.Len(t, taskList.Items, 1)
	assert.Equal(t, "Use exponential backoff for payment gateway retries", taskList.Items[0].Annotations["divergedev.com/guidance"])
	assert.NotEmpty(t, taskList.Items[0].Annotations["divergedev.com/guidance-timestamp"])

	// 3. /diverge pause
	pauseCmd, _ := ParseSlashCommand("/diverge pause")
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", pauseCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Agent task paused")

	// 4. /diverge resume with budget bump
	resumeCmd, ok := ParseSlashCommand("/diverge resume +$15.50 50000")
	require.True(t, ok)
	assert.Equal(t, "15.50", resumeCmd.BudgetUSD)
	assert.Equal(t, int64(50000), resumeCmd.Tokens)

	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", resumeCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Agent task resumed")
	assert.Contains(t, feedback, "$15.50")
	assert.Contains(t, feedback, "50000")

	var task v1alpha1.AgentTask
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: taskList.Items[0].Name, Namespace: "default"}, &task))
	assert.False(t, task.Spec.Suspended)
	assert.Equal(t, "15.50", task.Spec.BudgetUSD)
	assert.Equal(t, int64(50000), task.Spec.BudgetTokens)

	// Second resume with additional budget should be additive
	resume2, _ := ParseSlashCommand("/diverge resume +$5.00")
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", resume2, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "$20.50")

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: taskList.Items[0].Name, Namespace: "default"}, &task))
	assert.Equal(t, "20.50", task.Spec.BudgetUSD)

	// 5. Pause task again, then use /diverge budget: must NOT un-suspend the task
	_, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", pauseCmd, repoURL, branch, prNumber)
	require.NoError(t, err)

	budgetCmd, ok := ParseSlashCommand("/diverge budget +$10.00 25000")
	require.True(t, ok)
	assert.Equal(t, "budget", budgetCmd.Action)
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", budgetCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "budget updated")
	assert.Contains(t, feedback, "Task suspension state was not modified")

	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: taskList.Items[0].Name, Namespace: "default"}, &task))
	assert.True(t, task.Spec.Suspended, "Task must remain suspended after /diverge budget")
	assert.Equal(t, "30.50", task.Spec.BudgetUSD)
	assert.Equal(t, int64(75000), task.Spec.BudgetTokens)
}

func TestExecuteSlashCommand_StatusAndHelp(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	repoURL := "https://github.com/divergedev/demo-orders-api"
	branch := "feat/orders"
	prNumber := 101

	// Help command returns HelpMessage
	helpCmd, ok := ParseSlashCommand("/diverge help")
	require.True(t, ok)
	feedback, err := ExecuteSlashCommandWithFeedback(ctx, c, "default", helpCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Equal(t, HelpMessage, feedback)

	// Status when nothing is active
	statusCmd, ok := ParseSlashCommand("/diverge status")
	require.True(t, ok)
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", statusCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "No active agent tasks or preview environments found")

	// Create task and preview, then check status
	fixCmd, _ := ParseSlashCommand("/diverge fix implement catalog filter")
	_, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", fixCmd, repoURL, branch, prNumber)
	require.NoError(t, err)

	previewCmd, _ := ParseSlashCommand("/diverge preview")
	_, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", previewCmd, repoURL, branch, prNumber)
	require.NoError(t, err)

	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", statusCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Autonomous Agent Tasks")
	assert.Contains(t, feedback, "implement catalog filter")
	assert.Contains(t, feedback, "Preview Environments")
}

func TestExecuteSlashCommand_VerifyAndReplay(t *testing.T) {
	ctx := context.Background()
	c := newTestClient()

	repoURL := "https://github.com/divergedev/demo-checkout-api"
	branch := "feat/checkout"
	prNumber := 77

	// 1. Verify and replay before preview exists returns friendly warning
	verifyCmd, ok := ParseSlashCommand("/diverge verify /healthz")
	require.True(t, ok)
	feedback, err := ExecuteSlashCommandWithFeedback(ctx, c, "default", verifyCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "No active preview environment found")

	// 2. Stand up httptest gateway that routes based on x-diverge-env
	var (
		mu          sync.Mutex
		seenHeaders = make(map[string][]string)
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdrVal := r.Header.Get("x-diverge-env")
		mu.Lock()
		seenHeaders[r.URL.Path] = append(seenHeaders[r.URL.Path], hdrVal)
		mu.Unlock()

		isPreview := hdrVal == "pr-77"
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		if isPreview {
			_, _ = w.Write([]byte(`{"status":"ok","env":"preview","items":3}`))
		} else {
			_, _ = w.Write([]byte(`{"status":"ok","env":"baseline","items":2}`))
		}
	}))
	defer srv.Close()

	// Create and populate Environment with Running status and URL
	previewCmd, _ := ParseSlashCommand("/diverge preview")
	_, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", previewCmd, repoURL, branch, prNumber)
	require.NoError(t, err)

	envName := previewEnvName("pr", "divergedev/demo-checkout-api", prNumber)
	var env v1alpha1.Environment
	require.NoError(t, c.Get(ctx, types.NamespacedName{Name: envName, Namespace: "default"}, &env))
	env.Spec.Routing = v1alpha1.EnvironmentRouting{
		Mode:        "header",
		HeaderKey:   "x-diverge-env",
		HeaderValue: "pr-77",
	}
	env.Status.Phase = v1alpha1.PhaseRunning
	env.Status.URL = srv.URL + "/?_diverge_env=pr-77"
	require.NoError(t, c.Update(ctx, &env))

	// 3. Execute /diverge verify /healthz
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", verifyCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Diverge Verify Passed")
	assert.Contains(t, feedback, "Workload Diagnostics")
	assert.Contains(t, feedback, "/healthz")
	mu.Lock()
	assert.ElementsMatch(t, []string{"", "pr-77"}, seenHeaders["/healthz"])
	seenHeaders = make(map[string][]string)
	mu.Unlock()

	// 4. Execute /diverge replay /api/v1/checkout /healthz
	replayCmd, ok := ParseSlashCommand("/diverge replay /api/v1/checkout /healthz")
	require.True(t, ok)
	feedback, err = ExecuteSlashCommandWithFeedback(ctx, c, "default", replayCmd, repoURL, branch, prNumber)
	require.NoError(t, err)
	assert.Contains(t, feedback, "Diverge Traffic Replay Passed")
	assert.Contains(t, feedback, "/api/v1/checkout")
	assert.Contains(t, feedback, "/healthz")
	mu.Lock()
	assert.ElementsMatch(t, []string{"", "pr-77"}, seenHeaders["/api/v1/checkout"])
	assert.ElementsMatch(t, []string{"", "pr-77"}, seenHeaders["/healthz"])
	mu.Unlock()
}

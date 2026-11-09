package webhook

import (
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"net/url"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/doctor"
	"github.com/divergedev/diverge/pkg/replay"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	defaultExtendTTLDuration = 24 * time.Hour
	prNumberAnnotationKey    = "divergedev.com/pr-number"
)

// HelpMessage is the user-facing markdown reference of supported /diverge slash commands.
const HelpMessage = `### 🤖 Diverge Slash Commands Guide

Use ` + "`/diverge <command>`" + ` in any PR or MR comment to manage preview environments and autonomous agent tasks:

| Command | Description | Example |
|---|---|---|
| ` + "`/diverge fix <objective>`" + ` | Start autonomous code-test-review loop | ` + "`/diverge fix resolve nil pointer in payment handler`" + ` |
| ` + "`/diverge guide <instruction>`" + ` | Steer running agent with runtime guidance | ` + "`/diverge guide prioritize backward compatibility in auth.go`" + ` |
| ` + "`/diverge pause`" + ` | Suspend autonomous agent execution | ` + "`/diverge pause`" + ` |
| ` + "`/diverge resume [+$amount] [tokens]`" + ` | Resume agent with optional budget bump | ` + "`/diverge resume +$5.00`" + ` |
| ` + "`/diverge status`" + ` | Report active tasks and preview environments | ` + "`/diverge status`" + ` |
| ` + "`/diverge preview`" + ` | Deploy preview environment for this branch | ` + "`/diverge preview`" + ` |
| ` + "`/diverge verify [path] [--fix]`" + ` | Run health diagnostics & HTTP probe against preview | ` + "`/diverge verify /healthz --fix`" + ` |
| ` + "`/diverge replay [path ...] [--fix]`" + ` | Replay baseline vs preview parity checks | ` + "`/diverge replay /api/v1/health /api/v1/status --fix`" + ` |
| ` + "`/diverge extend-ttl [duration]`" + ` | Extend preview TTL (default 24h) | ` + "`/diverge extend-ttl 48h`" + ` |
| ` + "`/diverge teardown`" + ` | Tear down preview environment & agent tasks | ` + "`/diverge teardown`" + ` |
| ` + "`/diverge help`" + ` | Show this command reference | ` + "`/diverge help`" + ` |
`

// SlashCommand represents a parsed /diverge command from a PR or MR comment.
type SlashCommand struct {
	Action    string
	Objective string
	Guidance  string
	Duration  time.Duration
	BudgetUSD string
	Tokens    int64
	Paths     []string
	AutoFix   bool
}

// ParseSlashCommand parses a PR/MR comment body for a /diverge slash command.
// Supported commands:
//   - /diverge fix <objective>
//   - /diverge guide <instruction>
//   - /diverge preview
//   - /diverge verify [path ...]
//   - /diverge replay [path ...]
//   - /diverge teardown
//   - /diverge extend-ttl [duration]
//   - /diverge pause
//   - /diverge resume [+$amount] [tokens]
//   - /diverge budget [+$amount] [tokens]
//   - /diverge status
//   - /diverge help
func ParseSlashCommand(commentBody string) (*SlashCommand, bool) {
	lines := strings.Split(commentBody, "\n")
	for _, rawLine := range lines {
		line := strings.TrimSpace(rawLine)
		if !strings.HasPrefix(line, "/diverge") {
			continue
		}
		rest := strings.TrimSpace(strings.TrimPrefix(line, "/diverge"))
		if rest == "" {
			continue
		}
		fields := strings.Fields(rest)
		subcmd := strings.ToLower(fields[0])
		argStr := strings.TrimSpace(rest[len(fields[0]):])

		switch subcmd {
		case "fix":
			if argStr == "" {
				return nil, false
			}
			return &SlashCommand{
				Action:    "fix",
				Objective: argStr,
			}, true
		case "guide":
			if argStr == "" {
				return nil, false
			}
			return &SlashCommand{
				Action:   "guide",
				Guidance: argStr,
			}, true
		case "preview":
			return &SlashCommand{
				Action: "preview",
			}, true
		case "verify", "replay":
			autoFix := false
			pathTokens := make([]string, 0, len(fields[1:]))
			for _, tok := range fields[1:] {
				switch strings.ToLower(strings.TrimSpace(tok)) {
				case "--fix", "--auto-fix":
					autoFix = true
				default:
					pathTokens = append(pathTokens, tok)
				}
			}
			paths, ok := parseSafeRelativePaths(pathTokens)
			if !ok {
				return nil, false
			}
			return &SlashCommand{
				Action:  subcmd,
				Paths:   paths,
				AutoFix: autoFix,
			}, true
		case "teardown":
			return &SlashCommand{
				Action: "teardown",
			}, true
		case "status":
			return &SlashCommand{
				Action: "status",
			}, true
		case "help":
			return &SlashCommand{
				Action: "help",
			}, true
		case "extend-ttl":
			dur := defaultExtendTTLDuration
			if argStr != "" {
				parsed, err := time.ParseDuration(fields[1])
				if err != nil || parsed <= 0 {
					return nil, false
				}
				dur = parsed
			}
			return &SlashCommand{
				Action:   "extend-ttl",
				Duration: dur,
			}, true
		case "pause":
			return &SlashCommand{
				Action: "pause",
			}, true
		case "resume", "budget":
			budgetUSD, tokens, err := parseResumeArgs(fields)
			if err != nil {
				return nil, false
			}
			return &SlashCommand{
				Action:    subcmd,
				BudgetUSD: budgetUSD,
				Tokens:    tokens,
			}, true
		}
	}
	return nil, false
}

// parseSafeRelativePaths validates and returns safe relative URL paths from slash command arguments.
func parseSafeRelativePaths(tokens []string) ([]string, bool) {
	if len(tokens) == 0 {
		return []string{"/"}, true
	}
	paths := make([]string, 0, len(tokens))
	for _, tok := range tokens {
		p := strings.TrimSpace(tok)
		lower := strings.ToLower(p)
		if p == "" ||
			!strings.HasPrefix(p, "/") ||
			strings.HasPrefix(p, "//") ||
			strings.Contains(p, "..") ||
			strings.Contains(p, "\\") ||
			strings.Contains(lower, "%2e") ||
			strings.Contains(lower, "%2f") ||
			strings.Contains(lower, "%5c") {
			return nil, false
		}
		unescaped, err := url.PathUnescape(p)
		if err != nil ||
			!strings.HasPrefix(unescaped, "/") ||
			strings.HasPrefix(unescaped, "//") ||
			strings.Contains(unescaped, "..") ||
			strings.Contains(unescaped, "\\") {
			return nil, false
		}
		paths = append(paths, p)
	}
	return paths, true
}

// parseResumeArgs parses optional budget and token increments from a resume or budget slash command.
func parseResumeArgs(fields []string) (string, int64, error) {
	if len(fields) <= 1 {
		return "", 0, nil
	}
	var budgetUSD string
	var tokens int64
	for _, f := range fields[1:] {
		fClean := strings.TrimSpace(f)
		if strings.HasPrefix(fClean, "+$") || strings.HasPrefix(fClean, "$") || strings.HasPrefix(fClean, "+") || strings.Contains(fClean, ".") {
			clean := strings.TrimPrefix(strings.TrimPrefix(strings.TrimPrefix(fClean, "+"), "$"), "+")
			val, err := strconv.ParseFloat(clean, 64)
			if err != nil || val <= 0 {
				return "", 0, fmt.Errorf("invalid budget amount %q", f)
			}
			budgetUSD = fmt.Sprintf("%.2f", val)
		} else if tok, err := strconv.ParseInt(fClean, 10, 64); err == nil && tok > 0 {
			tokens = tok
		} else {
			if val, err := strconv.ParseFloat(fClean, 64); err == nil && val > 0 {
				budgetUSD = fmt.Sprintf("%.2f", val)
			} else {
				return "", 0, fmt.Errorf("unrecognized resume argument %q", f)
			}
		}
	}
	return budgetUSD, tokens, nil
}

// ExecuteSlashCommand executes a parsed /diverge slash command against the Kubernetes cluster.
func ExecuteSlashCommand(ctx context.Context, c client.Client, ns string, cmd *SlashCommand, repoURL, branch string, prNumber int) error {
	_, err := ExecuteSlashCommandWithFeedback(ctx, c, ns, cmd, repoURL, branch, prNumber)
	return err
}

// ExecuteSlashCommandWithFeedback executes a parsed /diverge slash command and returns a user-facing Markdown feedback message.
func ExecuteSlashCommandWithFeedback(ctx context.Context, c client.Client, ns string, cmd *SlashCommand, repoURL, branch string, prNumber int) (string, error) {
	if cmd == nil {
		return "", fmt.Errorf("slash command must not be nil")
	}
	if ns == "" {
		ns = "default"
	}
	if branch == "" {
		branch = "main"
	}

	switch cmd.Action {
	case "fix":
		return executeFixCommand(ctx, c, ns, cmd, repoURL, branch, prNumber)
	case "guide":
		return executeGuideCommand(ctx, c, ns, cmd.Guidance, repoURL, prNumber)
	case "preview":
		return executePreviewCommand(ctx, c, ns, repoURL, branch, prNumber)
	case "verify":
		return executeVerifyCommand(ctx, c, ns, cmd.Paths, cmd.AutoFix, repoURL, branch, prNumber)
	case "replay":
		return executeReplayCommand(ctx, c, ns, cmd.Paths, cmd.AutoFix, repoURL, branch, prNumber)
	case "teardown":
		return executeTeardownCommand(ctx, c, ns, repoURL, prNumber)
	case "extend-ttl":
		return executeExtendTTLCommand(ctx, c, ns, cmd.Duration, repoURL, prNumber)
	case "pause":
		return executeSuspendTaskCommand(ctx, c, ns, repoURL, prNumber, true, "", 0)
	case "resume":
		return executeSuspendTaskCommand(ctx, c, ns, repoURL, prNumber, false, cmd.BudgetUSD, cmd.Tokens)
	case "budget":
		return executeBudgetCommand(ctx, c, ns, repoURL, prNumber, cmd.BudgetUSD, cmd.Tokens)
	case "status":
		return executeStatusCommand(ctx, c, ns, repoURL, prNumber)
	case "help":
		return HelpMessage, nil
	default:
		return "", fmt.Errorf("unsupported slash command action %q", cmd.Action)
	}
}

// executeFixCommand creates or updates an AgentTask for a /diverge fix slash command.
func executeFixCommand(ctx context.Context, c client.Client, ns string, cmd *SlashCommand, repoURL, branch string, prNumber int) (string, error) {
	if strings.TrimSpace(cmd.Objective) == "" {
		return "", fmt.Errorf("fix command requires a non-empty objective")
	}
	if repoURL == "" {
		repoURL = "https://github.com/unknown/repository"
	}

	taskName := buildCommandTaskName(repoURL, prNumber, cmd.Objective)
	prStr := strconv.Itoa(prNumber)
	workingBranch := fmt.Sprintf("agent/%s", taskName)
	if prNumber > 0 {
		workingBranch = fmt.Sprintf("pr-%d", prNumber)
	}

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: ns,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, c, task, func() error {
		if task.Labels == nil {
			task.Labels = make(map[string]string)
		}
		if task.Annotations == nil {
			task.Annotations = make(map[string]string)
		}
		task.Labels["divergedev.com/managed-by"] = "diverge-webhook"
		if prNumber > 0 {
			task.Labels[prNumberAnnotationKey] = prStr
			task.Annotations[prNumberAnnotationKey] = prStr
		}
		task.Spec.Objective = cmd.Objective
		task.Spec.Repository = v1alpha1.AgentTaskRepository{
			URL:           repoURL,
			BaseBranch:    branch,
			WorkingBranch: workingBranch,
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	feedback := fmt.Sprintf("🚀 **Agent task started**: `%s`\n\n"+
		"- **Objective**: %s\n"+
		"- **Working Branch**: `%s`\n\n"+
		"Follow execution progress via PR comments or stream logs with:\n"+
		"```bash\ndiverge task logs %s -f\n```",
		taskName, cmd.Objective, workingBranch, taskName)
	return feedback, nil
}

// executeGuideCommand attaches runtime guidance annotations to active AgentTasks for a PR/MR.
func executeGuideCommand(ctx context.Context, c client.Client, ns, guidance, repoURL string, prNumber int) (string, error) {
	if strings.TrimSpace(guidance) == "" {
		return "", fmt.Errorf("guide command requires non-empty guidance text")
	}

	var taskList v1alpha1.AgentTaskList
	if err := c.List(ctx, &taskList, client.InNamespace(ns)); err != nil {
		return "", err
	}

	prStr := strconv.Itoa(prNumber)
	var matchedTasks []*v1alpha1.AgentTask
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if task.Status.Phase == v1alpha1.AgentTaskPhaseCompleted || task.Status.Phase == v1alpha1.AgentTaskPhaseFailed {
			continue
		}
		if matchesTaskPR(task, repoURL, prNumber, prStr) {
			matchedTasks = append(matchedTasks, task)
		}
	}

	if len(matchedTasks) == 0 {
		return "⚠️ No active agent task found for this PR/MR. Use `/diverge fix <objective>` to start an autonomous task.", nil
	}

	nowStr := time.Now().UTC().Format(time.RFC3339)
	for _, task := range matchedTasks {
		if task.Annotations == nil {
			task.Annotations = make(map[string]string)
		}
		task.Annotations["divergedev.com/guidance"] = guidance
		task.Annotations["divergedev.com/guidance-timestamp"] = nowStr
		if err := c.Update(ctx, task); err != nil {
			return "", fmt.Errorf("update agent task guidance: %w", err)
		}
	}

	if len(matchedTasks) == 1 {
		return fmt.Sprintf("🧭 **Guidance saved** to agent task `%s`:\n\n> %s\n\nThis guidance will be delivered to the agent on its next iteration.", matchedTasks[0].Name, guidance), nil
	}
	return fmt.Sprintf("🧭 **Guidance saved** to %d agent tasks:\n\n> %s\n\nThis guidance will be delivered to the agents on their next iteration.", len(matchedTasks), guidance), nil
}

// executePreviewCommand provisions or updates an Environment CR for a PR/MR.
func executePreviewCommand(ctx context.Context, c client.Client, ns, repoURL, branch string, prNumber int) (string, error) {
	project := projectFromRepoURL(repoURL)
	provider := providerFromRepoURL(repoURL)
	prefix := "pr"
	if provider == "gitlab" {
		prefix = "mr"
	}
	envName := previewEnvName(prefix, project, prNumber)
	prStr := strconv.Itoa(prNumber)

	env := &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envName,
			Namespace: ns,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, c, env, func() error {
		if env.Labels == nil {
			env.Labels = make(map[string]string)
		}
		if env.Annotations == nil {
			env.Annotations = make(map[string]string)
		}
		SetTraceparentAnnotation(env, TraceparentFromContext(ctx))
		if prNumber > 0 {
			env.Labels[prNumberAnnotationKey] = prStr
			env.Annotations[prNumberAnnotationKey] = prStr
		}
		env.Spec.Source = v1alpha1.EnvironmentSource{
			Provider: provider,
			Project:  project,
			MR:       prNumber,
			Branch:   branch,
		}
		return nil
	})
	if err != nil {
		return "", err
	}

	feedback := fmt.Sprintf("🔄 **Preview environment requested**: `%s` (branch: `%s`)\n\nDiverge is provisioning your preview stack.", envName, branch)
	return feedback, nil
}

// resolvePreviewTarget resolves the preview environment name, base URL, preview URL, routing mode, and routing header key/value for a PR/MR.
func resolvePreviewTarget(ctx context.Context, c client.Client, ns, repoURL string, prNumber int) (envName, baseURL, previewURL, routingMode, headerKey, headerValue string, found bool, err error) {
	defaultBaseline := stripPreviewQueryParam(os.Getenv("DIVERGE_BASE_URL"))
	envs, err := findMatchingEnvironments(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", "", "", "", "", "", false, err
	}
	if len(envs) > 0 {
		env := envs[0]
		for i := range envs {
			if strings.TrimSpace(envs[i].Status.URL) != "" {
				env = envs[i]
				break
			}
		}
		envName = env.Name
		routingMode = strings.TrimSpace(env.Spec.Routing.Mode)
		headerKey = replay.DefaultHeaderKey
		if env.Spec.Routing.HeaderKey != "" {
			headerKey = env.Spec.Routing.HeaderKey
		}
		headerValue = env.Name
		if env.Spec.Routing.HeaderValue != "" {
			headerValue = env.Spec.Routing.HeaderValue
		}
		statusClean := stripPreviewQueryParam(env.Status.URL)
		extClean := ""
		if ext := strings.TrimSpace(env.Spec.Routing.ExternalURL); ext != "" && !strings.Contains(ext, "{") {
			extClean = stripPreviewQueryParam(ext)
		}
		if strings.EqualFold(routingMode, "subdomain") {
			previewURL = statusClean
			if extClean != "" && !strings.EqualFold(extClean, previewURL) {
				baseURL = extClean
			} else if defaultBaseline != "" {
				baseURL = defaultBaseline
			}
		} else {
			if statusClean != "" {
				baseURL = statusClean
			} else if extClean != "" {
				baseURL = extClean
			} else if defaultBaseline != "" {
				baseURL = defaultBaseline
			}
			previewURL = baseURL
		}
		return envName, baseURL, previewURL, routingMode, headerKey, headerValue, true, nil
	}

	pgs, err := findMatchingPreviewGroups(ctx, c, repoURL, prNumber)
	if err != nil {
		return "", "", "", "", "", "", false, err
	}
	if len(pgs) == 0 {
		return "", "", "", "", "", "", false, nil
	}
	if len(pgs) > 1 {
		return "", "", "", "", "", "", false, fmt.Errorf("multiple PreviewGroups (%d) matched PR/MR #%d; cannot unambiguously resolve preview target", len(pgs), prNumber)
	}

	pg := pgs[0]
	envName = pg.Name
	routingMode = strings.TrimSpace(pg.Spec.Routing.Mode)
	headerKey = replay.DefaultHeaderKey
	if pg.Spec.Routing.HeaderKey != "" {
		headerKey = pg.Spec.Routing.HeaderKey
	}
	headerValue = pg.Name
	if pg.Spec.Routing.HeaderValue != "" {
		headerValue = pg.Spec.Routing.HeaderValue
	}
	extClean := ""
	if ext := strings.TrimSpace(pg.Spec.Routing.ExternalURL); ext != "" && !strings.Contains(ext, "{") {
		extClean = stripPreviewQueryParam(ext)
	}
	svcClean := ""
	if len(pg.Status.Services) == 1 && strings.TrimSpace(pg.Status.Services[0].URL) != "" {
		svcClean = stripPreviewQueryParam(pg.Status.Services[0].URL)
	}
	if strings.EqualFold(routingMode, "subdomain") {
		previewURL = svcClean
		if previewURL == "" {
			previewURL = extClean
		}
		if extClean != "" && !strings.EqualFold(extClean, previewURL) {
			baseURL = extClean
		} else if defaultBaseline != "" {
			baseURL = defaultBaseline
		}
	} else {
		if extClean != "" {
			baseURL = extClean
		} else if svcClean != "" {
			baseURL = svcClean
		} else if defaultBaseline != "" {
			baseURL = defaultBaseline
		}
		previewURL = baseURL
	}
	return envName, baseURL, previewURL, routingMode, headerKey, headerValue, true, nil
}

// stripPreviewQueryParam removes the _diverge_env sticky-routing query parameter from a preview URL.
func stripPreviewQueryParam(rawURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(rawURL))
	if err != nil {
		return strings.TrimSpace(rawURL)
	}
	q := parsed.Query()
	q.Del("_diverge_env")
	parsed.RawQuery = q.Encode()
	return strings.TrimRight(parsed.String(), "/")
}

// executeVerifyCommand runs cluster workload diagnostics and HTTP parity/health probes for a PR/MR preview environment.
func executeVerifyCommand(ctx context.Context, c client.Client, ns string, paths []string, autoFix bool, repoURL, branch string, prNumber int) (string, error) {
	envName, baseURL, previewURL, routingMode, headerKey, headerValue, found, err := resolvePreviewTarget(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	if !found {
		return "⚠️ No active preview environment found for this PR/MR. Run `/diverge preview` first.", nil
	}
	if len(paths) == 0 {
		paths = []string{"/"}
	}

	diagnoser := doctor.NewDiagnoser(c)
	docReport, docErr := diagnoser.Diagnose(ctx, ns, envName)

	var b strings.Builder
	docHealthy := docErr == nil && docReport != nil && docReport.Healthy
	replayPassed := false
	var repReport *replay.Report
	subdomainRouted := strings.EqualFold(routingMode, "subdomain")
	canProbeHTTP := baseURL != "" && (!subdomainRouted || previewURL != "")
	if canProbeHTTP {
		entries := make([]replay.RequestEntry, 0, len(paths))
		for _, p := range paths {
			entries = append(entries, replay.RequestEntry{
				ID:     fmt.Sprintf("GET %s", p),
				Method: "GET",
				Path:   p,
			})
		}
		repReport, err = replay.NewRunner(replay.Config{
			Environment:      envName,
			BaseURL:          baseURL,
			PreviewBaseURL:   previewURL,
			HeaderKey:        headerKey,
			HeaderValue:      headerValue,
			FailOnStatusDiff: true,
			Timeout:          5 * time.Second,
		}).Run(ctx, entries)
		if err == nil && repReport != nil {
			for i := range repReport.Results {
				r := &repReport.Results[i]
				if r.PreviewStatus < 200 || r.PreviewStatus >= 400 {
					if r.Passed {
						r.Passed = false
						repReport.PassedRequests--
						repReport.FailedRequests++
					}
					r.FailureReasons = append(r.FailureReasons, fmt.Sprintf("preview returned unhealthy HTTP %d", r.PreviewStatus))
				}
			}
			repReport.Passed = repReport.TotalRequests > 0 && repReport.FailedRequests == 0
			replayPassed = repReport.Passed
		}
	}

	overallPassed := docHealthy && replayPassed
	if overallPassed {
		fmt.Fprintf(&b, "### ✅ Diverge Verify Passed: `%s`\n\n", envName)
	} else {
		fmt.Fprintf(&b, "### ❌ Diverge Verify Failed: `%s`\n\n", envName)
	}

	if docErr != nil {
		fmt.Fprintf(&b, "- **Workload Diagnostics**: ❌ Error (`%v`)\n", docErr)
	} else if docReport != nil && docReport.Healthy {
		b.WriteString("- **Workload Diagnostics**: ✅ All preview workloads, migrations, and routes healthy\n")
	} else if docReport != nil {
		fmt.Fprintf(&b, "- **Workload Diagnostics**: ❌ %d issue(s) detected\n", len(docReport.Issues))
	}

	if subdomainRouted && !canProbeHTTP {
		b.WriteString("- **HTTP Probe**: ⏭️ Skipped (subdomain-routed environment requires an explicit baseline URL via `spec.routing.externalUrl` or `DIVERGE_BASE_URL`)\n")
		return b.String(), nil
	}
	if baseURL == "" {
		b.WriteString("- **HTTP Probe**: ⏭️ Skipped (preview URL not yet assigned in `status.url`)\n")
		return b.String(), nil
	}

	if err != nil {
		fmt.Fprintf(&b, "- **HTTP Probe**: ❌ Error (`%v`)\n", err)
		return b.String(), nil
	}

	if repReport != nil {
		b.WriteString("\n")
		var mdBuf bytes.Buffer
		_ = replay.FormatMarkdown(&mdBuf, repReport)
		b.WriteString(mdBuf.String())
	}

	if !overallPassed && autoFix {
		taskName, fixErr := spawnWebhookAutoFixTask(ctx, c, ns, envName, "verify", b.String(), repoURL, branch, prNumber)
		if fixErr == nil && taskName != "" {
			fmt.Fprintf(&b, "\n🤖 **Self-Healing Triggered**: Spawned/updated autonomous agent task `%s` to investigate and fix this failure.\n", taskName)
		}
	}
	return b.String(), nil
}

// executeReplayCommand runs deterministic baseline-vs-preview HTTP parity checks across the requested paths for a PR/MR preview environment.
func executeReplayCommand(ctx context.Context, c client.Client, ns string, paths []string, autoFix bool, repoURL, branch string, prNumber int) (string, error) {
	envName, baseURL, previewURL, routingMode, headerKey, headerValue, found, err := resolvePreviewTarget(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	if !found {
		return "⚠️ No active preview environment found for this PR/MR. Run `/diverge preview` first.", nil
	}
	subdomainRouted := strings.EqualFold(routingMode, "subdomain")
	if subdomainRouted && (baseURL == "" || previewURL == "") {
		return fmt.Sprintf("⚠️ Preview environment `%s` uses subdomain routing, which requires an explicit baseline URL (`spec.routing.externalUrl`, `DIVERGE_BASE_URL`, or `diverge replay %s --base-url <baseline-url>`).", envName, envName), nil
	}
	if baseURL == "" {
		return fmt.Sprintf("⚠️ Preview environment `%s` does not have an active target URL yet (`status.url` is empty).", envName), nil
	}
	if len(paths) == 0 {
		paths = []string{"/"}
	}

	entries := make([]replay.RequestEntry, 0, len(paths))
	for _, p := range paths {
		entries = append(entries, replay.RequestEntry{
			ID:     fmt.Sprintf("GET %s", p),
			Method: "GET",
			Path:   p,
		})
	}

	report, err := replay.NewRunner(replay.Config{
		Environment:        envName,
		BaseURL:            baseURL,
		PreviewBaseURL:     previewURL,
		HeaderKey:          headerKey,
		HeaderValue:        headerValue,
		FailOnStatusDiff:   true,
		FailOnContractDiff: true,
		Timeout:            10 * time.Second,
	}).Run(ctx, entries)
	if err != nil {
		return "", err
	}

	var mdBuf bytes.Buffer
	if err := replay.FormatMarkdown(&mdBuf, report); err != nil {
		return "", err
	}
	if !report.Passed && autoFix {
		taskName, fixErr := spawnWebhookAutoFixTask(ctx, c, ns, envName, "replay", mdBuf.String(), repoURL, branch, prNumber)
		if fixErr == nil && taskName != "" {
			fmt.Fprintf(&mdBuf, "\n🤖 **Self-Healing Triggered**: Spawned/updated autonomous agent task `%s` to investigate and fix this regression.\n", taskName)
		}
	}
	return mdBuf.String(), nil
}

// spawnWebhookAutoFixTask creates or updates an AgentTask seeded with a verification or replay failure summary.
func spawnWebhookAutoFixTask(ctx context.Context, c client.Client, ns, envName, failureKind, summary, repoURL, branch string, prNumber int) (string, error) {
	if repoURL == "" {
		repoURL = "https://github.com/unknown/repository"
	}
	objective := fmt.Sprintf("Fix %s regression on preview environment %s:\n\n%s", failureKind, envName, strings.TrimSpace(summary))
	taskName := buildCommandTaskName(repoURL, prNumber, "autofix-"+failureKind+"-"+envName)
	prStr := strconv.Itoa(prNumber)
	workingBranch := fmt.Sprintf("agent/%s", taskName)
	if prNumber > 0 {
		workingBranch = fmt.Sprintf("pr-%d", prNumber)
	}

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      taskName,
			Namespace: ns,
		},
	}
	_, err := ctrl.CreateOrUpdate(ctx, c, task, func() error {
		if task.Labels == nil {
			task.Labels = make(map[string]string)
		}
		if task.Annotations == nil {
			task.Annotations = make(map[string]string)
		}
		task.Labels["divergedev.com/managed-by"] = "diverge-webhook"
		task.Labels["diverge.io/autofix"] = failureKind
		if prNumber > 0 {
			task.Labels[prNumberAnnotationKey] = prStr
			task.Annotations[prNumberAnnotationKey] = prStr
		}
		task.Annotations["divergedev.com/guidance"] = objective
		task.Spec.Objective = objective
		task.Spec.Suspended = false
		task.Spec.Repository = v1alpha1.AgentTaskRepository{
			URL:           repoURL,
			BaseBranch:    branch,
			WorkingBranch: workingBranch,
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	return taskName, nil
}

// executeTeardownCommand deletes all matching Environments, PreviewGroups, and AgentTasks for a PR/MR.
func executeTeardownCommand(ctx context.Context, c client.Client, ns, repoURL string, prNumber int) (string, error) {
	envs, err := findMatchingEnvironments(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	for i := range envs {
		if err := client.IgnoreNotFound(c.Delete(ctx, &envs[i])); err != nil {
			return "", err
		}
	}

	pgs, err := findMatchingPreviewGroups(ctx, c, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	for i := range pgs {
		if err := client.IgnoreNotFound(c.Delete(ctx, &pgs[i])); err != nil {
			return "", err
		}
	}

	var taskList v1alpha1.AgentTaskList
	if err := c.List(ctx, &taskList, client.InNamespace(ns)); err != nil {
		return "", err
	}
	prStr := strconv.Itoa(prNumber)
	deletedTasks := 0
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if matchesTaskPR(task, repoURL, prNumber, prStr) {
			if err := client.IgnoreNotFound(c.Delete(ctx, task)); err != nil {
				return "", err
			}
			deletedTasks++
		}
	}

	feedback := fmt.Sprintf("🧹 **Teardown requested**: Cleaned up %d preview environment(s), %d preview group(s), and %d agent task(s) for this PR/MR.", len(envs), len(pgs), deletedTasks)
	return feedback, nil
}

// executeExtendTTLCommand extends the lifecycle TTL on all matching Environments and PreviewGroups for a PR/MR.
func executeExtendTTLCommand(ctx context.Context, c client.Client, ns string, dur time.Duration, repoURL string, prNumber int) (string, error) {
	if dur <= 0 {
		dur = defaultExtendTTLDuration
	}

	envs, err := findMatchingEnvironments(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	for i := range envs {
		env := &envs[i]
		env.Spec.Lifecycle.TTL = &metav1.Duration{Duration: dur}
		if err := c.Update(ctx, env); err != nil {
			return "", err
		}
	}

	pgs, err := findMatchingPreviewGroups(ctx, c, repoURL, prNumber)
	if err != nil {
		return "", err
	}
	for i := range pgs {
		pg := &pgs[i]
		if pg.Spec.Lifecycle == nil {
			pg.Spec.Lifecycle = &v1alpha1.PreviewGroupLifecycle{}
		}
		pg.Spec.Lifecycle.TTL = &metav1.Duration{Duration: dur}
		if err := c.Update(ctx, pg); err != nil {
			return "", err
		}
	}

	feedback := fmt.Sprintf("⏳ **Preview TTL extended** by %s for %d environment(s).", dur, len(envs)+len(pgs))
	return feedback, nil
}

// executeSuspendTaskCommand pauses or resumes active AgentTasks for a PR/MR, optionally increasing budget and token limits.
func executeSuspendTaskCommand(ctx context.Context, c client.Client, ns, repoURL string, prNumber int, suspend bool, budgetUSD string, tokens int64) (string, error) {
	var taskList v1alpha1.AgentTaskList
	if err := c.List(ctx, &taskList, client.InNamespace(ns)); err != nil {
		return "", err
	}

	prStr := strconv.Itoa(prNumber)
	var matchedTasks []*v1alpha1.AgentTask
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if matchesTaskPR(task, repoURL, prNumber, prStr) {
			matchedTasks = append(matchedTasks, task)
		}
	}

	if len(matchedTasks) == 0 {
		if suspend {
			return "⚠️ No active agent task found to pause for this PR/MR.", nil
		}
		return "⚠️ No active agent task found to resume for this PR/MR.", nil
	}

	for _, task := range matchedTasks {
		task.Spec.Suspended = suspend
		if !suspend && budgetUSD != "" {
			currBudget := 0.0
			if task.Spec.BudgetUSD != "" {
				clean := strings.TrimPrefix(strings.TrimSpace(task.Spec.BudgetUSD), "$")
				if val, err := strconv.ParseFloat(clean, 64); err == nil {
					currBudget = val
				}
			}
			inc, _ := strconv.ParseFloat(budgetUSD, 64)
			newBudget := currBudget + inc
			task.Spec.BudgetUSD = fmt.Sprintf("%.2f", newBudget)
		}
		if !suspend && tokens > 0 {
			task.Spec.BudgetTokens += tokens
		}
		if !suspend && task.Spec.MaxIterations > 0 && task.Status.Iteration >= task.Spec.MaxIterations {
			task.Spec.MaxIterations = task.Status.Iteration + 5
		}
		if err := c.Update(ctx, task); err != nil {
			return "", err
		}
		if !suspend && (task.Status.Message != "" || len(task.Status.LastMeterRequestIDs) > 0) {
			task.Status.Message = ""
			task.Status.LastMeterRequestIDs = nil
			_ = c.Status().Update(ctx, task)
		}
	}

	taskName := matchedTasks[0].Name
	if suspend {
		return fmt.Sprintf("⏸️ **Agent task paused**: `%s`\n\nExecution suspended. Resume anytime with `/diverge resume` or `/diverge resume +$5`.", taskName), nil
	}

	var extra []string
	if budgetUSD != "" {
		newBudget, _ := strconv.ParseFloat(strings.TrimPrefix(matchedTasks[0].Spec.BudgetUSD, "$"), 64)
		extra = append(extra, fmt.Sprintf("**Budget**: `$%.2f` (+`$%s` added)", newBudget, budgetUSD))
	}
	if tokens > 0 {
		extra = append(extra, fmt.Sprintf("**Tokens**: +%d added", tokens))
	}
	extraStr := ""
	if len(extra) > 0 {
		extraStr = "\n- " + strings.Join(extra, "\n- ")
	}
	return fmt.Sprintf("▶️ **Agent task resumed**: `%s`%s\n\nThe autonomous loop will continue execution.", taskName, extraStr), nil
}

// executeBudgetCommand increases budget and token limits on matching AgentTasks without altering their suspension state.
func executeBudgetCommand(ctx context.Context, c client.Client, ns, repoURL string, prNumber int, budgetUSD string, tokens int64) (string, error) {
	var taskList v1alpha1.AgentTaskList
	if err := c.List(ctx, &taskList, client.InNamespace(ns)); err != nil {
		return "", fmt.Errorf("list agent tasks: %w", err)
	}

	prStr := strconv.Itoa(prNumber)
	var matchedTasks []*v1alpha1.AgentTask
	for i := range taskList.Items {
		task := &taskList.Items[i]
		if matchesTaskPR(task, repoURL, prNumber, prStr) {
			matchedTasks = append(matchedTasks, task)
		}
	}

	if len(matchedTasks) == 0 {
		return "⚠️ No active agent task found to adjust budget for this PR/MR.", nil
	}

	for _, task := range matchedTasks {
		if budgetUSD != "" {
			currBudget := 0.0
			if task.Spec.BudgetUSD != "" {
				clean := strings.TrimPrefix(strings.TrimSpace(task.Spec.BudgetUSD), "$")
				if val, err := strconv.ParseFloat(clean, 64); err == nil {
					currBudget = val
				}
			}
			inc, _ := strconv.ParseFloat(budgetUSD, 64)
			newBudget := currBudget + inc
			task.Spec.BudgetUSD = fmt.Sprintf("%.2f", newBudget)
		}
		if tokens > 0 {
			task.Spec.BudgetTokens += tokens
		}
		if err := c.Update(ctx, task); err != nil {
			return "", err
		}
	}

	taskName := matchedTasks[0].Name
	var extra []string
	if budgetUSD != "" {
		newBudget, _ := strconv.ParseFloat(strings.TrimPrefix(matchedTasks[0].Spec.BudgetUSD, "$"), 64)
		extra = append(extra, fmt.Sprintf("**Budget**: `$%.2f` (+`$%s` added)", newBudget, budgetUSD))
	}
	if tokens > 0 {
		extra = append(extra, fmt.Sprintf("**Tokens**: `%d` (+%d added)", matchedTasks[0].Spec.BudgetTokens, tokens))
	}
	extraStr := ""
	if len(extra) > 0 {
		extraStr = "\n- " + strings.Join(extra, "\n- ")
	}
	return fmt.Sprintf("💰 **Agent task budget updated**: `%s`%s\n\nTask suspension state was not modified.", taskName, extraStr), nil
}

// executeStatusCommand formats a Markdown summary of active AgentTasks and Environments for a PR/MR.
func executeStatusCommand(ctx context.Context, c client.Client, ns, repoURL string, prNumber int) (string, error) {
	var taskList v1alpha1.AgentTaskList
	if err := c.List(ctx, &taskList, client.InNamespace(ns)); err != nil {
		return "", fmt.Errorf("list agent tasks: %w", err)
	}

	envs, err := findMatchingEnvironments(ctx, c, ns, repoURL, prNumber)
	if err != nil {
		return "", fmt.Errorf("list environments: %w", err)
	}

	prStr := strconv.Itoa(prNumber)
	var matchedTasks []v1alpha1.AgentTask
	for _, task := range taskList.Items {
		if matchesTaskPR(&task, repoURL, prNumber, prStr) {
			matchedTasks = append(matchedTasks, task)
		}
	}

	if len(matchedTasks) == 0 && len(envs) == 0 {
		return "ℹ️ No active agent tasks or preview environments found for this PR/MR.\n\nUse `/diverge fix <objective>` to start an agent task or `/diverge preview` to launch a preview environment.", nil
	}

	var b strings.Builder
	b.WriteString("### 📊 Diverge Status\n\n")

	if len(matchedTasks) > 0 {
		b.WriteString("#### 🤖 Autonomous Agent Tasks\n\n")
		b.WriteString("| Task | Phase | Objective | Branch | LLM Spend | Suspended |\n")
		b.WriteString("|---|---|---|---|---|---|\n")
		for _, t := range matchedTasks {
			cost := t.Status.CostUSD
			if cost == "" {
				cost = "$0.00"
			}
			phase := string(t.Status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			susp := "No"
			if t.Spec.Suspended {
				susp = "Yes (Paused)"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s | `%s` | %s (%d tokens) | %s |\n",
				t.Name, phase, t.Spec.Objective, t.Spec.Repository.WorkingBranch, cost, t.Status.TokensConsumed, susp)
		}
		b.WriteString("\n")
	}

	if len(envs) > 0 {
		b.WriteString("#### 🌐 Preview Environments\n\n")
		b.WriteString("| Name | Status | Preview URL |\n")
		b.WriteString("|---|---|---|\n")
		for _, env := range envs {
			urlCell := "Provisioning..."
			if env.Status.URL != "" {
				urlCell = fmt.Sprintf("[🔗 Open Preview](%s)", env.Status.URL)
			}
			phase := string(env.Status.Phase)
			if phase == "" {
				phase = "Pending"
			}
			fmt.Fprintf(&b, "| `%s` | %s | %s |\n", env.Name, phase, urlCell)
		}
		b.WriteString("\n")
	}

	b.WriteString("💡 *Run `/diverge help` to view all available interactive slash commands.*")
	return b.String(), nil
}

// matchesTaskPR reports whether task belongs to the specified repository and PR/MR number.
func matchesTaskPR(task *v1alpha1.AgentTask, repoURL string, prNumber int, prStr string) bool {
	if repoURL != "" && normalizeRepoURL(task.Spec.Repository.URL) != normalizeRepoURL(repoURL) {
		return false
	}
	if prNumber > 0 {
		if task.Annotations[prNumberAnnotationKey] == prStr || task.Labels[prNumberAnnotationKey] == prStr {
			return true
		}
		if task.Spec.Repository.WorkingBranch == fmt.Sprintf("pr-%d", prNumber) {
			return true
		}
		return false
	}
	return repoURL != ""
}

// normalizeRepoURL normalizes a Git repository URL for case-insensitive comparison.
func normalizeRepoURL(raw string) string {
	return strings.TrimSuffix(strings.TrimSuffix(strings.ToLower(strings.TrimSpace(raw)), "/"), ".git")
}

// findMatchingEnvironments returns all Environment resources in ns associated with repoURL and prNumber.
func findMatchingEnvironments(ctx context.Context, c client.Client, ns, repoURL string, prNumber int) ([]v1alpha1.Environment, error) {
	var envList v1alpha1.EnvironmentList
	if err := c.List(ctx, &envList, client.InNamespace(ns)); err != nil {
		return nil, err
	}

	if prNumber <= 0 {
		return nil, nil
	}

	project := projectFromRepoURL(repoURL)
	expectedPRName := previewEnvName("pr", project, prNumber)
	expectedMRName := previewEnvName("mr", project, prNumber)
	prStr := strconv.Itoa(prNumber)

	var matched []v1alpha1.Environment
	for _, env := range envList.Items {
		if project != "" {
			if env.Spec.Source.Project != "" {
				if !strings.EqualFold(env.Spec.Source.Project, project) {
					continue
				}
			} else if env.Name != expectedPRName && env.Name != expectedMRName {
				continue
			}
		}
		if env.Spec.Source.MR == prNumber ||
			env.Annotations[prNumberAnnotationKey] == prStr ||
			env.Labels[prNumberAnnotationKey] == prStr ||
			env.Name == expectedPRName ||
			env.Name == expectedMRName {
			matched = append(matched, env)
		}
	}
	return matched, nil
}

// findMatchingPreviewGroups returns all PreviewGroup resources associated with repoURL and prNumber.
func findMatchingPreviewGroups(ctx context.Context, c client.Client, repoURL string, prNumber int) ([]v1alpha1.PreviewGroup, error) {
	var pgList v1alpha1.PreviewGroupList
	if err := c.List(ctx, &pgList); err != nil {
		if meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
			return nil, nil
		}
		return nil, err
	}

	if prNumber <= 0 {
		return nil, nil
	}

	project := projectFromRepoURL(repoURL)
	prStr := strconv.Itoa(prNumber)
	var matched []v1alpha1.PreviewGroup
	for _, pg := range pgList.Items {
		if project != "" && !strings.EqualFold(pg.Spec.Source.Project, project) {
			continue
		}
		if pg.Spec.Source.MR == prNumber ||
			pg.Annotations[prNumberAnnotationKey] == prStr ||
			pg.Labels[prNumberAnnotationKey] == prStr {
			matched = append(matched, pg)
		}
	}
	return matched, nil
}

// buildCommandTaskName constructs a deterministic Kubernetes resource name for an AgentTask.
func buildCommandTaskName(repoURL string, prNumber int, objective string) string {
	raw := fmt.Sprintf("task-pr-%d", prNumber)
	if prNumber <= 0 {
		raw = "task-fix"
	}
	raw = strings.ToLower(raw)
	raw = regexp.MustCompile(`[^a-z0-9-]`).ReplaceAllString(raw, "-")
	raw = strings.Trim(raw, "-")
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s/%d/%s", repoURL, prNumber, objective))))[:8]
	return fmt.Sprintf("%s-%s", raw, hash)
}

// projectFromRepoURL extracts the owner/repo project path from a repository URL.
func projectFromRepoURL(repoURL string) string {
	trimmed := strings.TrimSpace(repoURL)
	trimmed = strings.TrimSuffix(trimmed, ".git")
	if parsed, err := url.Parse(trimmed); err == nil && parsed.Host != "" {
		return strings.Trim(parsed.Path, "/")
	}
	return strings.Trim(trimmed, "/")
}

// providerFromRepoURL infers the SCM provider ("gitlab" or "github") from a repository URL.
func providerFromRepoURL(repoURL string) string {
	lower := strings.ToLower(repoURL)
	if strings.Contains(lower, "gitlab") {
		return "gitlab"
	}
	return "github"
}

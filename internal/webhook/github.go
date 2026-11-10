package webhook

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/internal/deployer"
	"github.com/divergedev/diverge/internal/metrics"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"
)

type GitHubWebhookHandler struct {
	Client        client.Client
	Config        WebhookConfig
	ConfigFetcher ConfigFetcher
	Commenter     CommentPoster
	HTTPClient    *http.Client
	DefaultNS     string
	Dedup         *DeliveryDedup
}

type GitHubPRPayload struct {
	Action      string `json:"action"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
			SHA string `json:"sha"`
		} `json:"head"`
		Base struct {
			Ref string `json:"ref"`
		} `json:"base"`
		State string `json:"state"`
	} `json:"pull_request"`
	Repository struct {
		FullName string `json:"full_name"`
	} `json:"repository"`
}

func (h *GitHubWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceparent := ResolveTraceparent(r)
	ctx := WithTraceparent(r.Context(), traceparent)
	logger := log.FromContext(ctx).WithName("github-webhook")

	start := time.Now()
	action := "unknown"
	defer func() {
		metrics.WebhookProcessDuration.WithLabelValues("github", action).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 5<<20) // 5MB limit

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if strings.TrimSpace(h.Config.SecretToken) == "" {
		logger.Info("Webhook secret token is not configured")
		http.Error(w, "webhook secret token is not configured", http.StatusUnauthorized)
		return
	}

	signature := r.Header.Get("X-Hub-Signature-256")
	if signature == "" {
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	expectedMAC := hmac.New(sha256.New, []byte(h.Config.SecretToken))
	expectedMAC.Write(bodyBytes)
	expectedSignature := "sha256=" + hex.EncodeToString(expectedMAC.Sum(nil))

	if !hmac.Equal([]byte(signature), []byte(expectedSignature)) {
		logger.Info("Unauthorized webhook request")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Dedup check AFTER authentication to prevent cache poisoning
	deliveryID := r.Header.Get("X-GitHub-Delivery")
	if h.Dedup != nil && h.Dedup.IsDuplicate(deliveryID) {
		logger.Info("Duplicate webhook event received", "deliveryID", deliveryID)
		w.WriteHeader(http.StatusOK)
		return
	}

	event := r.Header.Get("X-GitHub-Event")
	if event == "issue_comment" {
		action = "issue_comment"
		h.handleIssueComment(ctx, w, bodyBytes)
		return
	}
	if event != "pull_request" {
		w.WriteHeader(http.StatusOK)
		return
	}

	var payload GitHubPRPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		logger.Error(err, "Failed to decode payload")
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	action = normalizeGitHubAction(payload.Action)

	logger.Info("Received GitHub PR event", "pr", payload.PullRequest.Number, "action", payload.Action)

	switch payload.Action {
	case "opened", "reopened", "synchronize":
		if err := h.reconcileEnvironment(ctx, &payload); err != nil {
			logger.Error(err, "Failed to reconcile environment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	case "closed":
		if err := h.deleteEnvironment(ctx, &payload); err != nil {
			logger.Error(err, "Failed to delete environment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

type GitHubIssueCommentPayload struct {
	Action string `json:"action"`
	Issue  struct {
		Number      int `json:"number"`
		PullRequest *struct {
			URL string `json:"url"`
		} `json:"pull_request,omitempty"`
	} `json:"issue"`
	PullRequest struct {
		Number int `json:"number"`
		Head   struct {
			Ref string `json:"ref"`
		} `json:"head"`
	} `json:"pull_request"`
	Comment struct {
		Body              string `json:"body"`
		AuthorAssociation string `json:"author_association"`
	} `json:"comment"`
	Repository struct {
		FullName      string `json:"full_name"`
		CloneURL      string `json:"clone_url"`
		HTMLURL       string `json:"html_url"`
		DefaultBranch string `json:"default_branch"`
	} `json:"repository"`
}

func isAuthorizedGitHubCommenter(assoc string) bool {
	switch strings.ToUpper(strings.TrimSpace(assoc)) {
	case "OWNER", "MEMBER", "COLLABORATOR":
		return true
	default:
		return false
	}
}

func (h *GitHubWebhookHandler) handleIssueComment(ctx context.Context, w http.ResponseWriter, bodyBytes []byte) {
	logger := log.FromContext(ctx).WithName("github-webhook")

	var payload GitHubIssueCommentPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		logger.Error(err, "Failed to decode issue_comment payload")
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if payload.Action != "" && payload.Action != "created" {
		w.WriteHeader(http.StatusOK)
		return
	}

	if payload.Issue.PullRequest == nil && payload.PullRequest.Number == 0 {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !isAuthorizedGitHubCommenter(payload.Comment.AuthorAssociation) {
		logger.Info("Ignoring slash command from unauthorized GitHub commenter", "author_association", payload.Comment.AuthorAssociation)
		w.WriteHeader(http.StatusOK)
		return
	}

	cmd, ok := ParseSlashCommand(payload.Comment.Body)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	prNumber := payload.Issue.Number
	if prNumber == 0 {
		prNumber = payload.PullRequest.Number
	}

	repoURL := payload.Repository.CloneURL
	if repoURL == "" && payload.Repository.HTMLURL != "" {
		repoURL = payload.Repository.HTMLURL
	}
	if repoURL == "" && payload.Repository.FullName != "" {
		repoURL = "https://github.com/" + payload.Repository.FullName
	}

	branch := strings.TrimSpace(payload.PullRequest.Head.Ref)
	if branch == "" && payload.Issue.PullRequest != nil && payload.Issue.PullRequest.URL != "" {
		branch = h.fetchGitHubPRHeadRef(ctx, payload.Issue.PullRequest.URL)
	}
	if branch == "" && h.Client != nil && payload.Repository.FullName != "" && prNumber > 0 {
		ns := h.DefaultNS
		if ns == "" {
			ns = "default"
		}
		var existingEnv v1alpha1.Environment
		envName := previewEnvName("pr", payload.Repository.FullName, prNumber)
		if err := h.Client.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &existingEnv); err == nil && existingEnv.Spec.Source.Branch != "" {
			branch = existingEnv.Spec.Source.Branch
		}
	}
	if branch == "" {
		branch = payload.Repository.DefaultBranch
	}
	if branch == "" {
		branch = "main"
	}

	var (
		execErr  error
		feedback string
	)
	if cmd.Action == "fix" {
		feedback, execErr = h.handleFixCommand(ctx, &payload, cmd, repoURL, branch, prNumber)
	} else {
		feedback, execErr = ExecuteSlashCommandWithFeedback(ctx, h.Client, h.DefaultNS, cmd, repoURL, branch, prNumber)
	}
	if execErr != nil {
		logger.Error(execErr, "Failed to execute slash command", "action", cmd.Action, "pr", prNumber)
		h.postFeedbackComment(ctx, payload.Repository.FullName, prNumber, fmt.Sprintf("❌ **Diverge Command Error**\n\nFailed to execute `/diverge %s`: %v\n\nRun `/diverge help` for a list of available commands.", cmd.Action, execErr))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if feedback != "" {
		h.postFeedbackComment(ctx, payload.Repository.FullName, prNumber, feedback)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitHubWebhookHandler) handleFixCommand(ctx context.Context, payload *GitHubIssueCommentPayload, cmd *SlashCommand, repoURL, branch string, prNumber int) (string, error) {
	feedback, err := ExecuteSlashCommandWithFeedback(ctx, h.Client, h.DefaultNS, cmd, repoURL, branch, prNumber)
	if err != nil {
		return "", err
	}
	if h.Client == nil {
		return feedback, nil
	}
	ns := h.DefaultNS
	if ns == "" {
		ns = "default"
	}
	effectiveRepoURL := repoURL
	if effectiveRepoURL == "" {
		effectiveRepoURL = "https://github.com/unknown/repository"
	}
	taskName := buildCommandTaskName(effectiveRepoURL, prNumber, cmd.Objective)
	var task v1alpha1.AgentTask
	if err := h.Client.Get(ctx, client.ObjectKey{Name: taskName, Namespace: ns}, &task); err != nil {
		return feedback, err
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Annotations["divergedev.com/provider"] = "github"
	if payload != nil && payload.Repository.FullName != "" {
		task.Annotations["divergedev.com/project"] = payload.Repository.FullName
	}
	if prNumber > 0 {
		task.Annotations["divergedev.com/pr-number"] = strconv.Itoa(prNumber)
	}
	SetTraceparentAnnotation(&task, TraceparentFromContext(ctx))
	if err := h.Client.Update(ctx, &task); err != nil {
		return feedback, err
	}
	return feedback, nil
}

func (h *GitHubWebhookHandler) postFeedbackComment(ctx context.Context, project string, prNumber int, body string) {
	if h.Commenter == nil || project == "" || prNumber <= 0 || strings.TrimSpace(body) == "" {
		return
	}
	logger := log.FromContext(ctx).WithName("github-webhook")
	if _, err := h.Commenter.PostComment(ctx, project, prNumber, body); err != nil {
		logger.Error(err, "Failed to post feedback comment to GitHub PR", "project", project, "pr", prNumber)
	}
}

func (h *GitHubWebhookHandler) fetchGitHubPRHeadRef(ctx context.Context, prAPIURL string) string {
	parsed, err := url.Parse(strings.TrimSpace(prAPIURL))
	if err != nil || (parsed.Scheme != "https" && parsed.Scheme != "http") {
		return ""
	}
	host := strings.ToLower(parsed.Hostname())
	if host != "api.github.com" && host != "localhost" && host != "127.0.0.1" && host != "::1" {
		return ""
	}

	httpClient := h.HTTPClient
	if httpClient == nil {
		if host == "api.github.com" && strings.TrimSpace(os.Getenv("GITHUB_TOKEN")) == "" {
			return ""
		}
		httpClient = &http.Client{Timeout: 5 * time.Second}
	}

	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, parsed.String(), nil)
	if err != nil {
		return ""
	}
	req.Header.Set("Accept", "application/vnd.github+json")
	if tok := strings.TrimSpace(os.Getenv("GITHUB_TOKEN")); tok != "" {
		req.Header.Set("Authorization", "Bearer "+tok)
	}

	resp, err := httpClient.Do(req)
	if err != nil {
		return ""
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return ""
	}

	var prData struct {
		Head struct {
			Ref string `json:"ref"`
		} `json:"head"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, 1<<20)).Decode(&prData); err != nil {
		return ""
	}
	return strings.TrimSpace(prData.Head.Ref)
}

func (h *GitHubWebhookHandler) reconcileEnvironment(ctx context.Context, payload *GitHubPRPayload) error {
	envName := previewEnvName("pr", payload.Repository.FullName, payload.PullRequest.Number)
	ns := h.DefaultNS
	if ns == "" {
		ns = "default"
	}

	env := &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envName,
			Namespace: ns,
		},
	}

	_, err := ctrl.CreateOrUpdate(ctx, h.Client, env, func() error {
		SetTraceparentAnnotation(env, TraceparentFromContext(ctx))
		env.Spec.Source = v1alpha1.EnvironmentSource{
			Provider:  "github",
			Project:   payload.Repository.FullName,
			MR:        payload.PullRequest.Number,
			Branch:    payload.PullRequest.Head.Ref,
			CommitSHA: payload.PullRequest.Head.SHA,
		}

		if h.ConfigFetcher != nil {
			cfgData, err := h.ConfigFetcher.FetchConfig(ctx, "github",
				payload.Repository.FullName,
				payload.PullRequest.Head.Ref)
			if err != nil {
				if strings.Contains(err.Error(), "not found") {
					env.Spec.ServiceConfig = nil
					env.Spec.Deploy.ChangedServices = nil
					env.Spec.Routing.HeaderKey = ""
				} else {
					return err
				}
			} else {
				cfg, parseErr := deployer.ParseDotDivergeConfig(cfgData)
				if parseErr != nil {
					return parseErr
				}
				image := cfg.PreviewImage(payload.Repository.FullName,
					safeSHA(payload.PullRequest.Head.SHA, 12))
				env.Spec.ServiceConfig = cfg.ToServicePreviewConfig(image)
				env.Spec.Deploy.ChangedServices = []string{cfg.Spec.ServiceName}
				env.Spec.Deploy.Namespace = "same"
				env.Spec.Routing.HeaderKey = cfg.Spec.Routing.HeaderKey
			}
		} else {
			env.Spec.ServiceConfig = nil
			env.Spec.Deploy.ChangedServices = nil
			env.Spec.Routing.HeaderKey = ""
		}

		return nil
	})
	return err
}

func (h *GitHubWebhookHandler) deleteEnvironment(ctx context.Context, payload *GitHubPRPayload) error {
	envName := previewEnvName("pr", payload.Repository.FullName, payload.PullRequest.Number)
	ns := h.DefaultNS
	if ns == "" {
		ns = "default"
	}

	env := &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      envName,
			Namespace: ns,
		},
	}
	return client.IgnoreNotFound(h.Client.Delete(ctx, env))
}

// normalizeGitHubAction maps webhook actions to a bounded set of known values
// to prevent unbounded Prometheus label cardinality.
func normalizeGitHubAction(action string) string {
	switch action {
	case "opened", "synchronize", "closed", "reopened", "edited", "ready_for_review":
		return action
	default:
		return "other"
	}
}

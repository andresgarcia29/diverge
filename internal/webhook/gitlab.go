package webhook

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
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

type WebhookConfig struct {
	SecretToken string
}

type GitLabWebhookHandler struct {
	Client        client.Client
	Config        WebhookConfig
	ConfigFetcher ConfigFetcher
	Commenter     CommentPoster
	AllowedUsers  []string
	DefaultNS     string
	Dedup         *DeliveryDedup
}

// GitLabMRPayload is a simple struct to decode MR webhook payloads
type GitLabMRPayload struct {
	ObjectKind       string `json:"object_kind"`
	EventType        string `json:"event_type"`
	ObjectAttributes struct {
		ID           int    `json:"id"`
		IID          int    `json:"iid"`
		TargetBranch string `json:"target_branch"`
		SourceBranch string `json:"source_branch"`
		State        string `json:"state"`
		Action       string `json:"action"`
		LastCommit   struct {
			ID string `json:"id"`
		} `json:"last_commit"`
	} `json:"object_attributes"`
	Project struct {
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
	} `json:"project"`
}

// GitLabNotePayload decodes GitLab Note Hook (MR comment) webhook payloads.
type GitLabNotePayload struct {
	ObjectKind string `json:"object_kind"`
	User       struct {
		ID       int    `json:"id"`
		Username string `json:"username"`
	} `json:"user"`
	ObjectAttributes struct {
		IID               int    `json:"iid"`
		SourceBranch      string `json:"source_branch"`
		Note              string `json:"note"`
		NoteableType      string `json:"noteable_type"`
		AuthorAssociation string `json:"author_association,omitempty"`
	} `json:"object_attributes"`
	MergeRequest struct {
		IID          int    `json:"iid"`
		SourceBranch string `json:"source_branch"`
		TargetBranch string `json:"target_branch"`
	} `json:"merge_request"`
	Project struct {
		Name              string `json:"name"`
		PathWithNamespace string `json:"path_with_namespace"`
		WebURL            string `json:"web_url"`
		GitHTTPURL        string `json:"git_http_url"`
	} `json:"project"`
}

// isAuthorizedGitLabCommenter returns true if the GitLab Note Hook commenter has a
// positive numeric user ID, satisfies any reported AuthorAssociation role check, and
// matches an immutable numeric user ID in the fail-closed AllowedUsers allowlist.
func (h *GitLabWebhookHandler) isAuthorizedGitLabCommenter(payload *GitLabNotePayload) bool {
	if payload == nil || payload.User.ID <= 0 {
		return false
	}
	if assoc := strings.ToUpper(strings.TrimSpace(payload.ObjectAttributes.AuthorAssociation)); assoc != "" {
		switch assoc {
		case "OWNER", "MEMBER", "COLLABORATOR", "MAINTAINER", "DEVELOPER":
		default:
			return false
		}
	}
	if len(h.AllowedUsers) == 0 {
		return false
	}
	userID := strconv.Itoa(payload.User.ID)
	for _, allowed := range h.AllowedUsers {
		if strings.TrimSpace(allowed) == userID {
			return true
		}
	}
	return false
}

func (h *GitLabWebhookHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	traceparent := ResolveTraceparent(r)
	ctx := WithTraceparent(r.Context(), traceparent)
	logger := log.FromContext(ctx).WithName("gitlab-webhook")

	start := time.Now()
	action := "unknown"
	defer func() {
		metrics.WebhookProcessDuration.WithLabelValues("gitlab", action).Observe(time.Since(start).Seconds())
	}()

	if r.Method != http.MethodPost {
		http.Error(w, "Method not allowed", http.StatusMethodNotAllowed)
		return
	}

	r.Body = http.MaxBytesReader(w, r.Body, 5<<20) // 5MB limit

	if !ValidateGitLabWebhookToken(r, h.Config.SecretToken) {
		logger.Info("Unauthorized webhook request")
		http.Error(w, "Unauthorized", http.StatusUnauthorized)
		return
	}

	// Dedup check AFTER authentication to prevent cache poisoning
	deliveryID := r.Header.Get("X-Gitlab-Event-UUID")
	if h.Dedup != nil && h.Dedup.IsDuplicate(deliveryID) {
		logger.Info("Duplicate webhook event received", "deliveryID", deliveryID)
		w.WriteHeader(http.StatusOK)
		return
	}

	bodyBytes, err := io.ReadAll(r.Body)
	if err != nil {
		logger.Error(err, "Failed to read request body")
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	var payload GitLabMRPayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		logger.Error(err, "Failed to decode payload")
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if payload.ObjectKind == "note" || r.Header.Get("X-Gitlab-Event") == "Note Hook" {
		action = "note"
		h.handleNoteEvent(ctx, w, bodyBytes)
		return
	}

	if payload.ObjectKind != "merge_request" {
		w.WriteHeader(http.StatusOK)
		return
	}

	action = normalizeGitLabAction(payload.ObjectAttributes.Action)

	logger.Info("Received GitLab MR event", "mr", payload.ObjectAttributes.IID, "action", payload.ObjectAttributes.Action)

	switch payload.ObjectAttributes.Action {
	case "open", "reopen", "update":
		if err := h.reconcileEnvironment(ctx, &payload); err != nil {
			logger.Error(err, "Failed to reconcile environment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	case "merge", "close":
		if err := h.deleteEnvironment(ctx, &payload); err != nil {
			logger.Error(err, "Failed to delete environment")
			http.Error(w, "Internal server error", http.StatusInternalServerError)
			return
		}
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitLabWebhookHandler) handleNoteEvent(ctx context.Context, w http.ResponseWriter, bodyBytes []byte) {
	logger := log.FromContext(ctx).WithName("gitlab-webhook")

	var payload GitLabNotePayload
	if err := json.Unmarshal(bodyBytes, &payload); err != nil {
		logger.Error(err, "Failed to decode note payload")
		http.Error(w, "Bad request", http.StatusBadRequest)
		return
	}

	if payload.ObjectAttributes.NoteableType != "MergeRequest" || (payload.MergeRequest.IID <= 0 && payload.ObjectAttributes.IID <= 0) {
		w.WriteHeader(http.StatusOK)
		return
	}

	if !h.isAuthorizedGitLabCommenter(&payload) {
		logger.Info("Ignoring slash command from unauthorized GitLab commenter", "userID", payload.User.ID, "username", payload.User.Username)
		w.WriteHeader(http.StatusOK)
		return
	}

	cmd, ok := ParseSlashCommand(payload.ObjectAttributes.Note)
	if !ok {
		w.WriteHeader(http.StatusOK)
		return
	}

	mrIID := payload.MergeRequest.IID
	if mrIID == 0 {
		mrIID = payload.ObjectAttributes.IID
	}

	repoURL := payload.Project.GitHTTPURL
	if repoURL == "" && payload.Project.WebURL != "" {
		repoURL = payload.Project.WebURL
	}
	if repoURL == "" && payload.Project.PathWithNamespace != "" {
		repoURL = "https://gitlab.com/" + payload.Project.PathWithNamespace
	}

	branch := strings.TrimSpace(payload.MergeRequest.SourceBranch)
	if branch == "" {
		branch = strings.TrimSpace(payload.ObjectAttributes.SourceBranch)
	}
	if branch == "" && h.Client != nil && payload.Project.PathWithNamespace != "" && mrIID > 0 {
		ns := h.DefaultNS
		if ns == "" {
			ns = "default"
		}
		var existingEnv v1alpha1.Environment
		envName := previewEnvName("mr", payload.Project.PathWithNamespace, mrIID)
		if err := h.Client.Get(ctx, client.ObjectKey{Name: envName, Namespace: ns}, &existingEnv); err == nil && existingEnv.Spec.Source.Branch != "" {
			branch = existingEnv.Spec.Source.Branch
		}
	}
	if branch == "" {
		branch = "main"
	}

	var (
		execErr  error
		feedback string
	)
	if cmd.Action == "fix" {
		feedback, execErr = h.handleFixCommand(ctx, &payload, cmd, repoURL, branch, mrIID)
	} else {
		feedback, execErr = ExecuteSlashCommandWithFeedback(ctx, h.Client, h.DefaultNS, cmd, repoURL, branch, mrIID)
	}
	if execErr != nil {
		logger.Error(execErr, "Failed to execute GitLab slash command", "action", cmd.Action, "mr", mrIID)
		h.postFeedbackComment(ctx, payload.Project.PathWithNamespace, mrIID, fmt.Sprintf("❌ **Diverge Command Error**\n\nFailed to execute `/diverge %s`: %v\n\nRun `/diverge help` for a list of available commands.", cmd.Action, execErr))
		http.Error(w, "Internal server error", http.StatusInternalServerError)
		return
	}

	if feedback != "" {
		h.postFeedbackComment(ctx, payload.Project.PathWithNamespace, mrIID, feedback)
	}

	w.WriteHeader(http.StatusOK)
}

func (h *GitLabWebhookHandler) handleFixCommand(ctx context.Context, payload *GitLabNotePayload, cmd *SlashCommand, repoURL, branch string, mrIID int) (string, error) {
	feedback, err := ExecuteSlashCommandWithFeedback(ctx, h.Client, h.DefaultNS, cmd, repoURL, branch, mrIID)
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
	taskName := buildCommandTaskName(effectiveRepoURL, mrIID, cmd.Objective)
	var task v1alpha1.AgentTask
	if err := h.Client.Get(ctx, client.ObjectKey{Name: taskName, Namespace: ns}, &task); err != nil {
		return feedback, err
	}
	if task.Annotations == nil {
		task.Annotations = make(map[string]string)
	}
	task.Annotations["divergedev.com/provider"] = "gitlab"
	if payload != nil && payload.Project.PathWithNamespace != "" {
		task.Annotations["divergedev.com/project"] = payload.Project.PathWithNamespace
	}
	if mrIID > 0 {
		mrStr := strconv.Itoa(mrIID)
		task.Annotations["divergedev.com/mr-iid"] = mrStr
		task.Annotations["divergedev.com/pr-number"] = mrStr
	}
	SetTraceparentAnnotation(&task, TraceparentFromContext(ctx))
	if err := h.Client.Update(ctx, &task); err != nil {
		return feedback, err
	}
	return feedback, nil
}

func (h *GitLabWebhookHandler) postFeedbackComment(ctx context.Context, project string, mrIID int, body string) {
	if h.Commenter == nil || project == "" || mrIID <= 0 || strings.TrimSpace(body) == "" {
		return
	}
	logger := log.FromContext(ctx).WithName("gitlab-webhook")
	if _, err := h.Commenter.PostComment(ctx, project, mrIID, body); err != nil {
		logger.Error(err, "Failed to post feedback comment to GitLab MR", "project", project, "mr", mrIID)
	}
}

func (h *GitLabWebhookHandler) reconcileEnvironment(ctx context.Context, payload *GitLabMRPayload) error {
	envName := previewEnvName("mr", payload.Project.PathWithNamespace, payload.ObjectAttributes.IID)
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
			Provider:  "gitlab",
			Project:   payload.Project.PathWithNamespace,
			MR:        payload.ObjectAttributes.IID,
			Branch:    payload.ObjectAttributes.SourceBranch,
			CommitSHA: payload.ObjectAttributes.LastCommit.ID,
		}

		if h.ConfigFetcher != nil {
			cfgData, err := h.ConfigFetcher.FetchConfig(ctx, "gitlab",
				payload.Project.PathWithNamespace,
				payload.ObjectAttributes.SourceBranch)
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
				image := fmt.Sprintf("%s:%s",
					payload.Project.PathWithNamespace,
					safeSHA(payload.ObjectAttributes.LastCommit.ID, 12))
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

func (h *GitLabWebhookHandler) deleteEnvironment(ctx context.Context, payload *GitLabMRPayload) error {
	envName := previewEnvName("mr", payload.Project.PathWithNamespace, payload.ObjectAttributes.IID)
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

// normalizeGitLabAction maps webhook actions to a bounded set of known values
// to prevent unbounded Prometheus label cardinality.
func normalizeGitLabAction(action string) string {
	switch action {
	case "open", "reopen", "update", "merge", "close", "approved", "unapproved":
		return action
	default:
		return "other"
	}
}

// ValidateGitLabWebhookToken validates a GitLab webhook payload token.
func ValidateGitLabWebhookToken(r *http.Request, expectedToken string) bool {
	token := r.Header.Get("X-Gitlab-Token")
	if strings.TrimSpace(expectedToken) == "" || strings.TrimSpace(token) == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(token), []byte(expectedToken)) == 1
}

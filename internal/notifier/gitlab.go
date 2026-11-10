package notifier

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/internal/metrics"
)

// GitLabNotifier implements Notifier by posting and updating merge request
// notes via the GitLab REST API v4.
type GitLabNotifier struct {
	BaseURL    string
	Token      string
	HTTPClient *http.Client
}

// NewGitLabNotifier creates a Notifier that posts and updates deployment status
// notes on GitLab merge requests via the GitLab REST API v4.
func NewGitLabNotifier(baseURL, token string) *GitLabNotifier {
	if baseURL == "" {
		baseURL = "https://gitlab.com"
	}
	return &GitLabNotifier{
		BaseURL: baseURL,
		Token:   token,
		HTTPClient: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func (g *GitLabNotifier) getProjectAndMR(env *v1alpha1.Environment) (string, int, error) {
	if env.Spec.Source.Provider != "gitlab" {
		return "", 0, fmt.Errorf("environment is not from gitlab")
	}
	return url.PathEscape(env.Spec.Source.Project), env.Spec.Source.MR, nil
}

func (g *GitLabNotifier) getCommentID(env *v1alpha1.Environment) int {
	return env.Status.CommentID
}

func (g *GitLabNotifier) setCommentID(env *v1alpha1.Environment, id int) {
	env.Status.CommentID = id
}

func (g *GitLabNotifier) postOrUpdateComment(ctx context.Context, env *v1alpha1.Environment, body string) error {
	project, mr, err := g.getProjectAndMR(env)
	if err != nil {
		return err
	}

	commentID := g.getCommentID(env)

	payload := map[string]string{"body": body}
	jsonPayload, _ := json.Marshal(payload)

	var reqURL string
	var method string

	if commentID != 0 {
		reqURL = fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes/%d", g.BaseURL, project, mr, commentID)
		method = http.MethodPut
	} else {
		reqURL = fmt.Sprintf("%s/api/v4/projects/%s/merge_requests/%d/notes", g.BaseURL, project, mr)
		method = http.MethodPost
	}

	req, err := http.NewRequestWithContext(ctx, method, reqURL, bytes.NewReader(jsonPayload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("PRIVATE-TOKEN", g.Token)

	resp, err := g.HTTPClient.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("gitlab API returned status: %d", resp.StatusCode)
	}

	if commentID == 0 {
		var result struct {
			ID int `json:"id"`
		}
		if err := json.NewDecoder(resp.Body).Decode(&result); err == nil && result.ID != 0 {
			g.setCommentID(env, result.ID)
		}
	}

	return nil
}

func buildTemplateData(env *v1alpha1.Environment, reason string) TemplateData {
	ttlStr := "never"
	if env.Spec.Lifecycle.TTL != nil {
		ttlStr = env.Spec.Lifecycle.TTL.Duration.String()
	}

	durationStr := "-"
	if env.Status.CreatedAt != nil {
		durationStr = time.Since(env.Status.CreatedAt.Time).String()
	}

	expiryStr := "never"
	if env.Status.ExpiresAt != nil {
		expiryStr = env.Status.ExpiresAt.Format(time.RFC3339)
	}

	var conditions []ConditionData
	var driftWarning string
	for _, c := range env.Status.Conditions {
		icon := "✅"
		if c.Status == "False" {
			icon = "❌"
		}
		conditions = append(conditions, ConditionData{
			Icon:    icon,
			Type:    c.Type,
			Message: c.Message,
		})
		if c.Type == "BaselineDriftDetected" && c.Status == metav1.ConditionTrue {
			msg := strings.TrimSpace(c.Message)
			if msg == "" {
				msg = "Baseline configuration has updated since this preview was deployed"
			}
			driftWarning = sanitizeMarkdown(msg)
		}
	}

	baseURL := ""
	if env.Status.URL != "" {
		baseURL = env.Status.URL
	} else if env.Spec.Routing.Mode == "subdomain" {
		baseURL = fmt.Sprintf("https://%s.preview.example.com", env.Name) // domain not fully known here, assume logic
	}

	headerKey := "x-diverge-env"
	if env.Spec.Routing.HeaderKey != "" {
		headerKey = sanitizeHeaderKey(env.Spec.Routing.HeaderKey)
	}

	headerVal := strings.TrimSpace(env.Spec.Routing.HeaderValue)
	if headerVal == "" {
		headerVal = env.Name
	}

	var dbSummary string
	if mode := strings.TrimSpace(env.Spec.Database.Mode); mode != "" && mode != "none" {
		dbSummary = fmt.Sprintf("`%s`", sanitizeMarkdown(mode))
		if seed := strings.TrimSpace(env.Spec.Database.SeedSource); seed != "" {
			dbSummary += fmt.Sprintf(" (seed: %s)", sanitizeMarkdown(seed))
		}
		if status := strings.TrimSpace(env.Status.DatabaseStatus); status != "" {
			dbSummary += fmt.Sprintf(" (%s)", sanitizeMarkdown(status))
		}
	}

	var asyncParts []string
	for _, route := range env.Spec.Routing.AsyncRoutes {
		if route.Protocol != "" && route.Target != "" {
			asyncParts = append(asyncParts, fmt.Sprintf("`%s:%s`", sanitizeMarkdown(route.Protocol), sanitizeMarkdown(route.Target)))
		}
	}
	if len(env.Status.AsyncEnvVars) > 0 {
		keys := make([]string, 0, len(env.Status.AsyncEnvVars))
		for k := range env.Status.AsyncEnvVars {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			asyncParts = append(asyncParts, fmt.Sprintf("`%s=%s`", sanitizeMarkdown(k), sanitizeMarkdown(env.Status.AsyncEnvVars[k])))
		}
	}
	asyncSummary := strings.Join(asyncParts, ", ")

	var costSummary string
	if env.Annotations != nil && env.Annotations[metrics.AnnotationHibernated] == "true" {
		costSummary = "💤 Hibernated ($0.000/hr)"
	} else {
		est := metrics.EstimateEnvironmentCost(env, time.Now(), metrics.DefaultCostRateCard())
		costSummary = fmt.Sprintf("$%.3f/hr (active)", est.HourlyUSD)
	}

	return TemplateData{
		Name:            env.Name,
		Branch:          env.Spec.Source.Branch,
		Mode:            env.Spec.Deploy.Mode,
		RoutingMode:     env.Spec.Routing.Mode,
		Services:        env.Spec.Deploy.ChangedServices,
		TTL:             ttlStr,
		URL:             env.Status.URL,
		NumServices:     len(env.Spec.Deploy.ChangedServices),
		Duration:        durationStr,
		BaseURL:         baseURL,
		ExpiryTime:      expiryStr,
		Reason:          reason,
		HeaderKey:       headerKey,
		HeaderValue:     headerVal,
		DatabaseSummary: dbSummary,
		AsyncSummary:    asyncSummary,
		CostSummary:     costSummary,
		DriftWarning:    driftWarning,
		Conditions:      conditions,
	}
}

// PostEnvironmentCreated posts a note indicating a new environment is being provisioned.
func (g *GitLabNotifier) PostEnvironmentCreated(ctx context.Context, env *v1alpha1.Environment) error {
	data := buildTemplateData(env, "")
	msg, err := renderTemplate(createdTemplate, data)
	if err != nil {
		return err
	}
	return g.postOrUpdateComment(ctx, env, msg)
}

// PostEnvironmentReady posts a note indicating the environment is ready with its preview URL.
func (g *GitLabNotifier) PostEnvironmentReady(ctx context.Context, env *v1alpha1.Environment) error {
	data := buildTemplateData(env, "")
	msg, err := renderTemplate(readyTemplate, data)
	if err != nil {
		return err
	}
	return g.postOrUpdateComment(ctx, env, msg)
}

// PostEnvironmentFailed posts a note indicating the environment deployment failed.
func (g *GitLabNotifier) PostEnvironmentFailed(ctx context.Context, env *v1alpha1.Environment, reason string) error {
	data := buildTemplateData(env, reason)
	msg, err := renderTemplate(failedTemplate, data)
	if err != nil {
		return err
	}
	return g.postOrUpdateComment(ctx, env, msg)
}

// PostEnvironmentTeardown posts a note indicating the environment is being torn down.
func (g *GitLabNotifier) PostEnvironmentTeardown(ctx context.Context, env *v1alpha1.Environment) error {
	data := buildTemplateData(env, "Teardown requested")
	msg, err := renderTemplate(teardownTemplate, data)
	if err != nil {
		return err
	}
	return g.postOrUpdateComment(ctx, env, msg)
}

// UpdateEnvironmentStatus re-renders and updates the existing note with current status.
func (g *GitLabNotifier) UpdateEnvironmentStatus(ctx context.Context, env *v1alpha1.Environment) error {
	data := buildTemplateData(env, "")
	msg, err := renderTemplate(readyTemplate, data)
	if err != nil {
		return err
	}
	return g.postOrUpdateComment(ctx, env, msg)
}

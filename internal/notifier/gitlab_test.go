package notifier

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func TestGitLabNotifier_LifecycleAndCommentOperations(t *testing.T) {
	ctx := context.Background()

	var methods []string
	var paths []string
	var bodies []string

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("PRIVATE-TOKEN") != "gl-secret-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		methods = append(methods, r.Method)
		paths = append(paths, r.URL.RawPath)
		if r.URL.RawPath == "" {
			paths[len(paths)-1] = r.URL.Path
		}

		var payload map[string]string
		_ = json.NewDecoder(r.Body).Decode(&payload)
		bodies = append(bodies, payload["body"])

		if r.Method == http.MethodPost {
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"id": 777}`))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id": 777}`))
	}))
	defer server.Close()

	notifier := NewGitLabNotifier(server.URL, "gl-secret-token")
	now := metav1.Now()
	expires := metav1.NewTime(now.Add(2 * time.Hour))

	env := &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name: "pr-101",
		},
		Spec: v1alpha1.EnvironmentSpec{
			Source: v1alpha1.EnvironmentSource{
				Provider: "gitlab",
				Project:  "group/sub/project",
				MR:       101,
				Branch:   "feat/new-ui",
			},
			Deploy: v1alpha1.EnvironmentDeploy{
				Mode:            "delta",
				ChangedServices: []string{"api", "web"},
			},
			Routing: v1alpha1.EnvironmentRouting{
				Mode:      "subdomain",
				HeaderKey: "x-custom-preview",
			},
			Lifecycle: v1alpha1.EnvironmentLifecycle{
				TTL: &metav1.Duration{Duration: 2 * time.Hour},
			},
		},
		Status: v1alpha1.EnvironmentStatus{
			CreatedAt: &now,
			ExpiresAt: &expires,
			Conditions: []metav1.Condition{
				{Type: "DatabaseReady", Status: metav1.ConditionTrue, Message: "Schema ready"},
				{Type: "DeployReady", Status: metav1.ConditionFalse, Message: "ImagePullBackOff"},
			},
		},
	}

	// 1. PostComment (PostEnvironmentCreated when CommentID == 0)
	require.NoError(t, notifier.PostEnvironmentCreated(ctx, env))
	assert.Equal(t, 777, env.Status.CommentID)
	assert.Equal(t, http.MethodPost, methods[0])
	assert.Equal(t, "/api/v4/projects/group%2Fsub%2Fproject/merge_requests/101/notes", paths[0])
	assert.Contains(t, bodies[0], "pr-101")
	assert.Contains(t, bodies[0], "feat/new-ui")

	// 2. UpdateComment (PostEnvironmentReady when CommentID != 0)
	env.Status.URL = "https://pr-101.preview.example.com"
	require.NoError(t, notifier.PostEnvironmentReady(ctx, env))
	assert.Equal(t, http.MethodPut, methods[1])
	assert.Equal(t, "/api/v4/projects/group%2Fsub%2Fproject/merge_requests/101/notes/777", paths[1])
	assert.Contains(t, bodies[1], "Ready!")
	assert.Contains(t, bodies[1], "x-custom-preview")

	// 3. PostEnvironmentFailed
	require.NoError(t, notifier.PostEnvironmentFailed(ctx, env, "deployment timeout"))
	assert.Equal(t, http.MethodPut, methods[2])
	assert.Contains(t, bodies[2], "deployment timeout")
	assert.Contains(t, bodies[2], "ImagePullBackOff")

	// 4. UpdateEnvironmentStatus
	require.NoError(t, notifier.UpdateEnvironmentStatus(ctx, env))
	assert.Equal(t, http.MethodPut, methods[3])

	// 5. PostEnvironmentTeardown
	require.NoError(t, notifier.PostEnvironmentTeardown(ctx, env))
	assert.Equal(t, http.MethodPut, methods[4])
	assert.Contains(t, bodies[4], "Teardown requested")
}

func TestGitLabNotifier_Errors(t *testing.T) {
	ctx := context.Background()

	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer server.Close()

	notifier := NewGitLabNotifier(server.URL, "token")

	// Wrong provider
	envWrong := &v1alpha1.Environment{
		Spec: v1alpha1.EnvironmentSpec{
			Source: v1alpha1.EnvironmentSource{
				Provider: "github",
				Project:  "owner/repo",
				MR:       1,
			},
		},
	}
	err := notifier.PostEnvironmentCreated(ctx, envWrong)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "not from gitlab")

	// Server error (500)
	envGL := &v1alpha1.Environment{
		Spec: v1alpha1.EnvironmentSpec{
			Source: v1alpha1.EnvironmentSource{
				Provider: "gitlab",
				Project:  "owner/repo",
				MR:       1,
			},
		},
	}
	err = notifier.PostEnvironmentCreated(ctx, envGL)
	require.Error(t, err)
	assert.Contains(t, err.Error(), "gitlab API returned status: 500")
}

func TestFormatPreviewGroupComment_TemplatesAndNoop(t *testing.T) {
	ctx := context.Background()
	noop := &NoopPreviewGroupNotifier{}
	pg := &v1alpha1.PreviewGroup{
		ObjectMeta: metav1.ObjectMeta{Name: "pg-feature-1"},
		Spec: v1alpha1.PreviewGroupSpec{
			Source: v1alpha1.EnvironmentSource{
				Provider: "gitlab",
				Project:  "org/repo",
				MR:       55,
				Branch:   "feat/multi-service",
			},
			Routing: v1alpha1.PreviewGroupRouting{
				HeaderKey:   "x-preview-group",
				HeaderValue: "55",
				ExternalURL: "https://preview.example.com",
			},
			Lifecycle: &v1alpha1.PreviewGroupLifecycle{
				TTL: &metav1.Duration{Duration: 4 * time.Hour},
			},
			Services: []v1alpha1.PreviewGroupServiceSpec{
				{Name: "api", Mode: v1alpha1.ServiceModeImage, Image: "ghcr.io/org/api:pr-55"},
				{Name: "worker", Mode: v1alpha1.ServiceModeLocal, Endpoint: "100.64.1.2:8080"},
				{Name: "auth", Mode: v1alpha1.ServiceModeBaseline},
			},
		},
		Status: v1alpha1.PreviewGroupStatus{
			Services: []v1alpha1.PreviewGroupServiceStatus{
				{Name: "api", Phase: v1alpha1.PhaseRunning, Namespace: "preview-55"},
				{Name: "worker", Phase: v1alpha1.PhaseFailed, Namespace: "preview-55", Reason: "CrashLoopBackOff"},
				{Name: "auth", Phase: v1alpha1.PhasePending, Namespace: "default"},
			},
		},
	}

	require.NoError(t, noop.PostGroupCreated(ctx, pg))
	require.NoError(t, noop.PostGroupReady(ctx, pg))
	require.NoError(t, noop.PostGroupFailed(ctx, pg, "worker failed"))
	require.NoError(t, noop.PostGroupTeardown(ctx, pg))
	require.NoError(t, noop.UpdateGroupStatus(ctx, pg))

	data := buildPreviewGroupTemplateData(pg, "worker crash")
	assert.Equal(t, "pg-feature-1", data.Name)
	assert.Equal(t, 3, data.ServiceCount)
	assert.Equal(t, 1, data.RunningCount)
	assert.Equal(t, "x-preview-group", data.HeaderKey)

	createdMsg, err := renderTemplate(pgCreatedTemplate, data)
	require.NoError(t, err)
	assert.Contains(t, createdMsg, "Diverge Preview — Deploying")
	assert.Contains(t, createdMsg, "100.64.1.2:8080")
	assert.Contains(t, createdMsg, "N/A (baseline)")

	readyMsg, err := renderTemplate(pgReadyTemplate, data)
	require.NoError(t, err)
	assert.Contains(t, readyMsg, "Diverge Preview — Ready!")
	assert.Contains(t, readyMsg, "https://preview.example.com")

	failedMsg, err := renderTemplate(pgFailedTemplate, data)
	require.NoError(t, err)
	assert.Contains(t, failedMsg, "Diverge Preview — Failed")
	assert.Contains(t, failedMsg, "worker crash")

	teardownMsg, err := renderTemplate(pgTeardownTemplate, data)
	require.NoError(t, err)
	assert.Contains(t, teardownMsg, "Diverge Preview — Destroyed")
}

func TestNewGitLabNotifierDefaultsBaseURL(t *testing.T) {
	assert.Equal(t, "https://gitlab.com", NewGitLabNotifier("", "token").BaseURL)
	assert.Equal(t, "https://gitlab.example.com", NewGitLabNotifier("https://gitlab.example.com", "token").BaseURL)
}

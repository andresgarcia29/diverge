package webhook

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/stretchr/testify/assert"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
)

func TestGitLabWebhookTokenValidation(t *testing.T) {
	fakeK8s := newTestClient()

	config := WebhookConfig{SecretToken: "secret-token"}
	handler := &GitLabWebhookHandler{Client: fakeK8s, Config: config, DefaultNS: "default"}

	tests := []struct {
		name       string
		token      string
		body       string
		statusCode int
	}{
		{
			name:       "valid token",
			token:      "secret-token",
			body:       `{"object_kind": "merge_request", "object_attributes": {"iid": 1, "action": "open", "source_branch": "feat/test", "last_commit": {"id": "abc123def456"}}, "project": {"path_with_namespace": "team/repo"}}`,
			statusCode: http.StatusOK,
		},
		{
			name:       "invalid token",
			token:      "wrong-token",
			body:       `{}`,
			statusCode: http.StatusUnauthorized,
		},
		{
			name:       "missing token",
			token:      "",
			body:       `{}`,
			statusCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tt.body))
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("X-Gitlab-Token", tt.token)

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.statusCode {
				t.Errorf("Expected status %d, got %d", tt.statusCode, rr.Code)
			}
		})
	}
}

func TestGitLabWebhookEmptySecretTokenRejected(t *testing.T) {
	fakeK8s := newTestClient()
	body := `{"object_kind": "merge_request", "object_attributes": {"iid": 1, "action": "open", "source_branch": "feat/test", "last_commit": {"id": "abc123def456"}}, "project": {"path_with_namespace": "team/repo"}}`

	for _, secret := range []string{"", "   "} {
		handler := &GitLabWebhookHandler{
			Client:    fakeK8s,
			Config:    WebhookConfig{SecretToken: secret},
			DefaultNS: "default",
		}
		req, err := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("X-Gitlab-Token", secret)

		if ValidateGitLabWebhookToken(req, secret) {
			t.Errorf("ValidateGitLabWebhookToken should return false for empty/whitespace secret %q", secret)
		}

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("secret=%q: expected status %d, got %d", secret, http.StatusUnauthorized, rr.Code)
		}
	}
}

func TestGitLabWebhookMalformedJSON(t *testing.T) {
	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{Config: config}

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{bad json`))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for malformed json, got %d", rr.Code)
	}
}

func TestGitLabWebhookUnsupportedEvent(t *testing.T) {
	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{Config: config}

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(`{"object_kind": "push"}`))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("Expected status 200 for unsupported event, got %d", rr.Code)
	}
}

func TestGitLabWebhookCreatesEnvironment(t *testing.T) {
	fakeK8s := newTestClient()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{
		Client:    fakeK8s,
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "open",
			"source_branch": "feat/new-payments",
			"last_commit": {"id": "abc123def456789"}
		},
		"project": {"path_with_namespace": "team/payments-api"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	// Verify CR contents
	env := &divergeiov1alpha1.Environment{}
	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "mr-0d7c4cc4-42", Namespace: "preview"}, env)
	if err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}
	if env.Spec.Source.Provider != "gitlab" {
		t.Errorf("Expected Provider gitlab, got %s", env.Spec.Source.Provider)
	}
	if env.Spec.Source.Project != "team/payments-api" {
		t.Errorf("Expected Project team/payments-api, got %s", env.Spec.Source.Project)
	}
	if env.Spec.Source.MR != 42 {
		t.Errorf("Expected MR 42, got %d", env.Spec.Source.MR)
	}
	if env.Spec.Source.Branch != "feat/new-payments" {
		t.Errorf("Expected Branch feat/new-payments, got %s", env.Spec.Source.Branch)
	}
}

func TestGitLabWebhookWithConfigFetcher(t *testing.T) {
	fakeK8s := newTestClient()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// editorconfig-checker-disable
		_, _ = w.Write([]byte(`apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: payment-api
spec:
  serviceName: payment-api
  namespace: my-namespace
  routing:
    headerKey: "x-custom-preview"
`))
		// editorconfig-checker-enable
	}))
	defer ts.Close()

	configFetcher := &GitLabConfigFetcher{
		BaseURL:    ts.URL,
		Token:      "secret-token",
		HTTPClient: ts.Client(),
	}

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{
		Client:        fakeK8s,
		Config:        config,
		ConfigFetcher: configFetcher,
		DefaultNS:     "preview",
	}

	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "open",
			"source_branch": "feat/new-payments",
			"last_commit": {"id": "abc123def456789"}
		},
		"project": {"path_with_namespace": "team/payments-api"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	// Verify CR contents with ConfigFetcher
	env := &divergeiov1alpha1.Environment{}
	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "mr-0d7c4cc4-42", Namespace: "preview"}, env)
	if err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}

	if env.Spec.ServiceConfig == nil {
		t.Fatalf("Expected ServiceConfig to be populated")
	}
	if env.Spec.ServiceConfig.ServiceName != "payment-api" {
		t.Errorf("Expected ServiceName payment-api, got %s", env.Spec.ServiceConfig.ServiceName)
	}
	if len(env.Spec.Deploy.ChangedServices) != 1 || env.Spec.Deploy.ChangedServices[0] != "payment-api" {
		t.Errorf("Expected ChangedServices [payment-api], got %v", env.Spec.Deploy.ChangedServices)
	}
}

func TestGitLabWebhookDeletesEnvironment(t *testing.T) {
	existingEnv := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "mr-7b9bcf5f-42",
			Namespace: "preview",
		},
	}
	fakeK8s := newTestClient(existingEnv)

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{
		Client:    fakeK8s,
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "merge",
			"source_branch": "feat/test",
			"last_commit": {"id": "abc123"}
		},
		"project": {"path_with_namespace": "team/repo"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "mr-7b9bcf5f-42", Namespace: "preview"}, &divergeiov1alpha1.Environment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Expected NotFound, got %v", err)
	}
}

func TestGitLabWebhookDeletesEnvironmentError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = divergeiov1alpha1.AddToScheme(scheme)
	fakeK8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{
		Client:    &errorClient{Client: fakeK8s},
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "merge",
			"source_branch": "feat/test",
			"last_commit": {"id": "abc123"}
		},
		"project": {"path_with_namespace": "team/repo"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("Expected 500, got %d", rr.Code)
	}
}

func TestGitLabWebhookDeletesOnMerge(t *testing.T) {
	fakeK8s := newTestClient()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitLabWebhookHandler{
		Client:    fakeK8s,
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "merge",
			"source_branch": "feat/test",
			"last_commit": {"id": "abc123"}
		},
		"project": {"path_with_namespace": "team/repo"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}
}

func TestGitLabWebhookNoteSlashCommand(t *testing.T) {
	fakeK8s := newTestClient()

	handler := &GitLabWebhookHandler{
		Client:       fakeK8s,
		Config:       WebhookConfig{SecretToken: "secret"},
		AllowedUsers: []string{"10"},
		DefaultNS:    "preview",
	}

	body := `{
		"object_kind": "note",
		"user": {
			"id": 10,
			"username": "maintainer"
		},
		"object_attributes": {
			"note": "/diverge fix update database connection pool timeout",
			"noteable_type": "MergeRequest"
		},
		"merge_request": {
			"iid": 88,
			"source_branch": "feat/db-pool"
		},
		"project": {
			"path_with_namespace": "team/payments-api",
			"git_http_url": "https://gitlab.com/team/payments-api.git"
		}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Note Hook")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	var tasks divergeiov1alpha1.AgentTaskList
	if err := fakeK8s.List(context.Background(), &tasks, client.InNamespace("preview")); err != nil {
		t.Fatalf("Failed to list AgentTasks: %v", err)
	}
	if len(tasks.Items) != 1 {
		t.Fatalf("Expected 1 AgentTask created, got %d", len(tasks.Items))
	}
	if tasks.Items[0].Spec.Objective != "update database connection pool timeout" {
		t.Errorf("Unexpected objective: %s", tasks.Items[0].Spec.Objective)
	}
	if tasks.Items[0].Annotations["divergedev.com/provider"] != "gitlab" {
		t.Errorf("Expected provider annotation gitlab, got %s", tasks.Items[0].Annotations["divergedev.com/provider"])
	}
	if tasks.Items[0].Annotations["divergedev.com/project"] != "team/payments-api" {
		t.Errorf("Expected project annotation team/payments-api, got %s", tasks.Items[0].Annotations["divergedev.com/project"])
	}
	if tasks.Items[0].Annotations["divergedev.com/mr-iid"] != "88" {
		t.Errorf("Expected mr-iid annotation 88, got %s", tasks.Items[0].Annotations["divergedev.com/mr-iid"])
	}
	if tasks.Items[0].Annotations["divergedev.com/pr-number"] != "88" {
		t.Errorf("Expected pr-number annotation 88, got %s", tasks.Items[0].Annotations["divergedev.com/pr-number"])
	}
	if !w3cTraceparentPattern.MatchString(tasks.Items[0].Annotations[TraceparentAnnotation]) {
		t.Errorf("Expected valid W3C traceparent annotation on AgentTask, got %q", tasks.Items[0].Annotations[TraceparentAnnotation])
	}
}

func TestGitLabWebhookTraceparentPropagation(t *testing.T) {
	fakeK8s := newTestClient()
	handler := &GitLabWebhookHandler{
		Client:    fakeK8s,
		Config:    WebhookConfig{SecretToken: "secret"},
		DefaultNS: "preview",
	}

	expectedTP := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	body := `{
		"object_kind": "merge_request",
		"object_attributes": {
			"iid": 42,
			"action": "open",
			"source_branch": "feat/new-payments",
			"last_commit": {"id": "abc123def456789"}
		},
		"project": {"path_with_namespace": "team/payments-api"}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("traceparent", expectedTP)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	env := &divergeiov1alpha1.Environment{}
	if err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "mr-0d7c4cc4-42", Namespace: "preview"}, env); err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}
	if env.Annotations[TraceparentAnnotation] != expectedTP {
		t.Errorf("Expected Environment traceparent %q, got %q", expectedTP, env.Annotations[TraceparentAnnotation])
	}
}

func TestGitLabWebhookNoteAuthorization(t *testing.T) {
	fakeK8s := newTestClient()

	handler := &GitLabWebhookHandler{
		Client:       fakeK8s,
		Config:       WebhookConfig{SecretToken: "secret"},
		AllowedUsers: []string{"1"},
		DefaultNS:    "preview",
	}

	cases := []struct {
		name string
		body string
	}{
		{
			name: "plain issue note is ignored",
			body: `{
				"object_kind": "note",
				"user": {"id": 1, "username": "trusted-dev"},
				"object_attributes": {
					"iid": 88,
					"note": "/diverge fix should not run on issues",
					"noteable_type": "Issue"
				},
				"project": {"path_with_namespace": "team/payments-api"}
			}`,
		},
		{
			name: "unauthorized user ID not in AllowedUsers is ignored even with matching username",
			body: `{
				"object_kind": "note",
				"user": {"id": 2, "username": "trusted-dev"},
				"object_attributes": {
					"note": "/diverge fix should not run for untrusted user",
					"noteable_type": "MergeRequest"
				},
				"merge_request": {"iid": 88, "source_branch": "feat/db-pool"},
				"project": {"path_with_namespace": "team/payments-api"}
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tc.body))
			req.Header.Set("X-Gitlab-Token", "secret")
			req.Header.Set("X-Gitlab-Event", "Note Hook")

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != http.StatusOK {
				t.Fatalf("Expected 200, got %d", rr.Code)
			}

			var tasks divergeiov1alpha1.AgentTaskList
			if err := fakeK8s.List(context.Background(), &tasks, client.InNamespace("preview")); err != nil {
				t.Fatalf("Failed to list AgentTasks: %v", err)
			}
			if len(tasks.Items) != 0 {
				t.Fatalf("Expected 0 AgentTasks created, got %d", len(tasks.Items))
			}
		})
	}

	t.Run("empty AllowedUsers fails closed", func(t *testing.T) {
		commenter := &testMockCommenter{}
		unconfiguredHandler := &GitLabWebhookHandler{
			Client:    fakeK8s,
			Config:    WebhookConfig{SecretToken: "secret"},
			Commenter: commenter,
			DefaultNS: "preview",
		}
		body := `{
			"object_kind": "note",
			"user": {"id": 1, "username": "trusted-dev"},
			"object_attributes": {
				"note": "/diverge replay /healthz",
				"noteable_type": "MergeRequest"
			},
			"merge_request": {"iid": 88, "source_branch": "feat/db-pool"},
			"project": {"path_with_namespace": "team/payments-api"}
		}`
		req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		req.Header.Set("X-Gitlab-Token", "secret")
		req.Header.Set("X-Gitlab-Event", "Note Hook")

		rr := httptest.NewRecorder()
		unconfiguredHandler.ServeHTTP(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("Expected 200, got %d", rr.Code)
		}
		if len(commenter.postedComments) != 0 {
			t.Fatalf("Expected 0 comments posted, got %d", len(commenter.postedComments))
		}
	})
}

func TestGitLabWebhookNoteWithCommenterFeedback(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = divergeiov1alpha1.AddToScheme(scheme)
	fakeK8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	commenter := &testMockCommenter{}
	handler := &GitLabWebhookHandler{
		Client:       fakeK8s,
		Config:       WebhookConfig{SecretToken: "secret"},
		Commenter:    commenter,
		AllowedUsers: []string{"1"},
		DefaultNS:    "preview",
	}

	body := `{
		"object_kind": "note",
		"user": {"id": 1, "username": "developer"},
		"object_attributes": {
			"iid": 99,
			"note": "/diverge status",
			"noteable_type": "MergeRequest"
		},
		"merge_request": {
			"iid": 99,
			"source_branch": "feat/api"
		},
		"project": {
			"path_with_namespace": "team/payments-api",
			"git_http_url": "https://gitlab.com/team/payments-api.git"
		}
	}`

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Gitlab-Token", "secret")
	req.Header.Set("X-Gitlab-Event", "Note Hook")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	if commenter.postedProject != "team/payments-api" {
		t.Errorf("Expected project team/payments-api, got %s", commenter.postedProject)
	}
	if commenter.postedMRID != 99 {
		t.Errorf("Expected MR ID 99, got %d", commenter.postedMRID)
	}
	if len(commenter.postedComments) != 1 {
		t.Fatalf("Expected 1 feedback comment posted, got %d", len(commenter.postedComments))
	}
	assert.Contains(t, commenter.postedComments[0], "No active agent tasks or preview environments found")
}

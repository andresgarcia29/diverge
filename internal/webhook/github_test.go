package webhook

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
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

func generateSignature(secret, body string) string {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(body))
	return "sha256=" + hex.EncodeToString(mac.Sum(nil))
}

func TestGitHubWebhookHMACValidation(t *testing.T) {
	fakeK8s := newTestClient()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{Client: fakeK8s, Config: config, DefaultNS: "default"}
	body := `{"action": "opened", "pull_request": {"number": 1, "head": {"ref": "feat/test", "sha": "abc123def456"}, "base": {"ref": "main"}}, "repository": {"full_name": "team/repo"}}`
	validSig := generateSignature("secret", body)

	tests := []struct {
		name       string
		signature  string
		statusCode int
	}{
		{
			name:       "valid signature",
			signature:  validSig,
			statusCode: http.StatusOK,
		},
		{
			name:       "invalid signature",
			signature:  "sha256=invalid",
			statusCode: http.StatusUnauthorized,
		},
		{
			name:       "missing signature",
			signature:  "",
			statusCode: http.StatusUnauthorized,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			if err != nil {
				t.Fatalf("Failed to create request: %v", err)
			}
			req.Header.Set("X-GitHub-Event", "pull_request")
			if tt.signature != "" {
				req.Header.Set("X-Hub-Signature-256", tt.signature)
			}

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			if rr.Code != tt.statusCode {
				t.Errorf("Expected status %d, got %d", tt.statusCode, rr.Code)
			}
		})
	}
}

func TestGitHubWebhookEmptySecretTokenRejected(t *testing.T) {
	fakeK8s := newTestClient()
	body := `{"action": "opened", "pull_request": {"number": 1, "head": {"ref": "feat/test", "sha": "abc123def456"}, "base": {"ref": "main"}}, "repository": {"full_name": "team/repo"}}`

	for _, secret := range []string{"", "   "} {
		handler := &GitHubWebhookHandler{
			Client:    fakeK8s,
			Config:    WebhookConfig{SecretToken: secret},
			DefaultNS: "default",
		}
		req, err := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
		if err != nil {
			t.Fatalf("Failed to create request: %v", err)
		}
		req.Header.Set("X-GitHub-Event", "pull_request")
		req.Header.Set("X-Hub-Signature-256", generateSignature(secret, body))

		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized {
			t.Errorf("secret=%q: expected status %d, got %d", secret, http.StatusUnauthorized, rr.Code)
		}
	}
}

func TestGitHubWebhookMalformedPayload(t *testing.T) {
	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{Config: config}

	body := `{bad json`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusBadRequest {
		t.Errorf("Expected status 400 for malformed json, got %d", rr.Code)
	}
}

func TestGitHubWebhookCreatesEnvironment(t *testing.T) {
	fakeK8s := newTestClient()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{
		Client:    fakeK8s,
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"action": "opened",
		"pull_request": {
			"number": 42,
			"head": {"ref": "feat/new-payments", "sha": "abc123def456789"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "divergedev/demo-payments-api"}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	// Verify CR contents
	env := &divergeiov1alpha1.Environment{}
	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "pr-661f4f5e-42", Namespace: "preview"}, env)
	if err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}
	if env.Spec.Source.Provider != "github" {
		t.Errorf("Expected Provider github, got %s", env.Spec.Source.Provider)
	}
	if env.Spec.Source.Project != "divergedev/demo-payments-api" {
		t.Errorf("Expected Project divergedev/demo-payments-api, got %s", env.Spec.Source.Project)
	}
	if env.Spec.Source.MR != 42 {
		t.Errorf("Expected MR 42, got %d", env.Spec.Source.MR)
	}
	if env.Spec.Source.Branch != "feat/new-payments" {
		t.Errorf("Expected Branch feat/new-payments, got %s", env.Spec.Source.Branch)
	}
}

type mockTransport struct {
	roundTripFunc func(req *http.Request) (*http.Response, error)
}

func (m *mockTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	return m.roundTripFunc(req)
}

func TestGitHubWebhookWithConfigFetcher(t *testing.T) {
	fakeK8s := newTestClient()

	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		// editorconfig-checker-disable
		_, _ = w.Write([]byte(`apiVersion: divergedev.com/v1alpha1
kind: ServicePreview
metadata:
  name: github-api
spec:
  serviceName: github-api
  namespace: my-namespace
`))
		// editorconfig-checker-enable
	}))
	defer ts.Close()

	configFetcher := &GitHubConfigFetcher{
		Token: "secret-token",
		HTTPClient: &http.Client{
			Transport: &mockTransport{
				roundTripFunc: func(req *http.Request) (*http.Response, error) {
					if req.URL.Query().Get("ref") != "feat/new-payments" {
						t.Errorf("Expected ref feat/new-payments, got %s", req.URL.Query().Get("ref"))
					}
					newURL, err := http.NewRequest(req.Method, ts.URL+req.URL.Path+"?"+req.URL.RawQuery, req.Body)
					if err != nil {
						return nil, err
					}
					newURL.Header = req.Header
					return ts.Client().Do(newURL)
				},
			},
		},
	}

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{
		Client:        fakeK8s,
		Config:        config,
		ConfigFetcher: configFetcher,
		DefaultNS:     "preview",
	}

	body := `{
		"action": "opened",
		"pull_request": {
			"number": 42,
			"head": {"ref": "feat/new-payments", "sha": "abc123def456789"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "divergedev/demo-payments-api"}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	env := &divergeiov1alpha1.Environment{}
	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "pr-661f4f5e-42", Namespace: "preview"}, env)
	if err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}

	if env.Spec.ServiceConfig == nil {
		t.Fatalf("Expected ServiceConfig to be populated")
	}
	if env.Spec.ServiceConfig.ServiceName != "github-api" {
		t.Errorf("Expected ServiceName github-api, got %s", env.Spec.ServiceConfig.ServiceName)
	}
	if len(env.Spec.Deploy.ChangedServices) != 1 || env.Spec.Deploy.ChangedServices[0] != "github-api" {
		t.Errorf("Expected ChangedServices [github-api], got %v", env.Spec.Deploy.ChangedServices)
	}
	if want := "divergedev/demo-payments-api:abc123def456"; env.Spec.ServiceConfig.Image != want {
		t.Errorf("Expected Image %s, got %s", want, env.Spec.ServiceConfig.Image)
	}
}

func TestGitHubWebhookDeletesEnvironment(t *testing.T) {
	existingEnv := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-661f4f5e-42",
			Namespace: "preview",
		},
	}
	fakeK8s := newTestClient(existingEnv)

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{
		Client:    fakeK8s,
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"action": "closed",
		"pull_request": {
			"number": 42,
			"head": {"ref": "feat/new-payments", "sha": "abc123def456789"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "divergedev/demo-payments-api"}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "pr-661f4f5e-42", Namespace: "preview"}, &divergeiov1alpha1.Environment{})
	if !apierrors.IsNotFound(err) {
		t.Fatalf("Expected NotFound, got %v", err)
	}
}

type errorClient struct {
	client.Client
}

func (e *errorClient) Delete(ctx context.Context, obj client.Object, opts ...client.DeleteOption) error {
	return context.DeadlineExceeded // simulate a generic error
}

func TestGitHubWebhookDeletesEnvironmentError(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = divergeiov1alpha1.AddToScheme(scheme)
	fakeK8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	config := WebhookConfig{SecretToken: "secret"}
	handler := &GitHubWebhookHandler{
		Client:    &errorClient{Client: fakeK8s},
		Config:    config,
		DefaultNS: "preview",
	}

	body := `{
		"action": "closed",
		"pull_request": {
			"number": 42,
			"head": {"ref": "feat/new-payments", "sha": "abc123def456789"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "divergedev/demo-payments-api"}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "pull_request")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusInternalServerError {
		t.Fatalf("Expected 500, got %d", rr.Code)
	}
}

func TestGitHubWebhookIssueCommentSlashCommand(t *testing.T) {
	fakeK8s := newTestClient()

	prServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"head":{"ref":"feat/race-fix"}}`))
	}))
	defer prServer.Close()

	handler := &GitHubWebhookHandler{
		Client:     fakeK8s,
		Config:     WebhookConfig{SecretToken: "secret"},
		HTTPClient: prServer.Client(),
		DefaultNS:  "preview",
	}

	body := `{
		"action": "created",
		"issue": {
			"number": 77,
			"pull_request": {"url": "` + prServer.URL + `/repos/divergedev/demo-api/pulls/77"}
		},
		"comment": {
			"body": "/diverge fix resolve race condition in worker pool",
			"author_association": "MEMBER"
		},
		"repository": {
			"full_name": "divergedev/demo-api",
			"clone_url": "https://github.com/divergedev/demo-api.git",
			"default_branch": "main"
		}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "issue_comment")

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
	if tasks.Items[0].Spec.Objective != "resolve race condition in worker pool" {
		t.Errorf("Unexpected objective: %s", tasks.Items[0].Spec.Objective)
	}
	if tasks.Items[0].Spec.Repository.BaseBranch != "feat/race-fix" {
		t.Errorf("Expected resolved PR head branch feat/race-fix, got %s", tasks.Items[0].Spec.Repository.BaseBranch)
	}
	if tasks.Items[0].Annotations["divergedev.com/provider"] != "github" {
		t.Errorf("Expected provider annotation github, got %s", tasks.Items[0].Annotations["divergedev.com/provider"])
	}
	if tasks.Items[0].Annotations["divergedev.com/project"] != "divergedev/demo-api" {
		t.Errorf("Expected project annotation divergedev/demo-api, got %s", tasks.Items[0].Annotations["divergedev.com/project"])
	}
	if tasks.Items[0].Annotations["divergedev.com/pr-number"] != "77" {
		t.Errorf("Expected pr-number annotation 77, got %s", tasks.Items[0].Annotations["divergedev.com/pr-number"])
	}
	if !w3cTraceparentPattern.MatchString(tasks.Items[0].Annotations[TraceparentAnnotation]) {
		t.Errorf("Expected valid W3C traceparent annotation on AgentTask, got %q", tasks.Items[0].Annotations[TraceparentAnnotation])
	}
}

func TestGitHubWebhookTraceparentPropagation(t *testing.T) {
	fakeK8s := newTestClient()
	handler := &GitHubWebhookHandler{
		Client:    fakeK8s,
		Config:    WebhookConfig{SecretToken: "secret"},
		DefaultNS: "preview",
	}

	expectedTP := "00-4bf92f3577b34da6a3ce929d0e0e4736-00f067aa0ba902b7-01"
	prBody := `{
		"action": "opened",
		"pull_request": {
			"number": 42,
			"head": {"ref": "feat/trace", "sha": "abc123def456789"},
			"base": {"ref": "main"}
		},
		"repository": {"full_name": "divergedev/demo-payments-api"}
	}`
	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(prBody))
	req.Header.Set("X-Hub-Signature-256", generateSignature("secret", prBody))
	req.Header.Set("X-GitHub-Event", "pull_request")
	req.Header.Set("traceparent", expectedTP)

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	env := &divergeiov1alpha1.Environment{}
	if err := fakeK8s.Get(context.Background(), client.ObjectKey{Name: "pr-661f4f5e-42", Namespace: "preview"}, env); err != nil {
		t.Fatalf("Expected Environment CR to be created, got %v", err)
	}
	if env.Annotations[TraceparentAnnotation] != expectedTP {
		t.Errorf("Expected Environment traceparent %q, got %q", expectedTP, env.Annotations[TraceparentAnnotation])
	}
}

func TestGitHubWebhookIssueCommentAuthorization(t *testing.T) {
	fakeK8s := newTestClient()

	handler := &GitHubWebhookHandler{
		Client:    fakeK8s,
		Config:    WebhookConfig{SecretToken: "secret"},
		DefaultNS: "preview",
	}

	cases := []struct {
		name string
		body string
	}{
		{
			name: "plain issue comment without pull_request is ignored",
			body: `{
				"action": "created",
				"issue": {"number": 77},
				"comment": {
					"body": "/diverge fix should not run on plain issues",
					"author_association": "OWNER"
				},
				"repository": {"full_name": "divergedev/demo-api"}
			}`,
		},
		{
			name: "untrusted commenter NONE is ignored",
			body: `{
				"action": "created",
				"issue": {
					"number": 77,
					"pull_request": {"url": "https://api.github.com/repos/divergedev/demo-api/pulls/77"}
				},
				"comment": {
					"body": "/diverge fix should not run for untrusted user",
					"author_association": "NONE"
				},
				"repository": {"full_name": "divergedev/demo-api"}
			}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig := generateSignature("secret", tc.body)
			req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(tc.body))
			req.Header.Set("X-Hub-Signature-256", sig)
			req.Header.Set("X-GitHub-Event", "issue_comment")

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
}

type testMockCommenter struct {
	postedComments []string
	postedProject  string
	postedMRID     int
}

func (m *testMockCommenter) PostComment(_ context.Context, project string, mrID int, body string) (int64, error) {
	m.postedProject = project
	m.postedMRID = mrID
	m.postedComments = append(m.postedComments, body)
	return int64(len(m.postedComments)), nil
}

func TestGitHubWebhookIssueCommentWithCommenterFeedback(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = divergeiov1alpha1.AddToScheme(scheme)
	fakeK8s := fake.NewClientBuilder().WithScheme(scheme).Build()

	commenter := &testMockCommenter{}
	handler := &GitHubWebhookHandler{
		Client:    fakeK8s,
		Config:    WebhookConfig{SecretToken: "secret"},
		Commenter: commenter,
		DefaultNS: "preview",
	}

	body := `{
		"action": "created",
		"issue": {
			"number": 88,
			"pull_request": {"url": "https://api.github.com/repos/divergedev/demo-api/pulls/88"}
		},
		"comment": {
			"body": "/diverge guide Avoid mutating shared pointers",
			"author_association": "COLLABORATOR"
		},
		"repository": {
			"full_name": "divergedev/demo-api",
			"clone_url": "https://github.com/divergedev/demo-api.git",
			"default_branch": "main"
		}
	}`
	sig := generateSignature("secret", body)

	req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
	req.Header.Set("X-Hub-Signature-256", sig)
	req.Header.Set("X-GitHub-Event", "issue_comment")

	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("Expected 200, got %d", rr.Code)
	}

	if commenter.postedProject != "divergedev/demo-api" {
		t.Errorf("Expected project divergedev/demo-api, got %s", commenter.postedProject)
	}
	if commenter.postedMRID != 88 {
		t.Errorf("Expected MR/PR 88, got %d", commenter.postedMRID)
	}
	if len(commenter.postedComments) != 1 {
		t.Fatalf("Expected 1 feedback comment posted, got %d", len(commenter.postedComments))
	}
	assert.Contains(t, commenter.postedComments[0], "No active agent task found")
}

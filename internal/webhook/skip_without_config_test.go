package webhook

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/internal/config"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

type staticConfigFetcher struct {
	data  []byte
	err   error
	calls int
}

func (f *staticConfigFetcher) FetchConfig(_ context.Context, _, _, _ string) ([]byte, error) {
	f.calls++
	return f.data, f.err
}

var errFetchServerError = fmt.Errorf("unexpected status %d fetching .diverge.yaml", http.StatusInternalServerError)

type skipWithoutConfigCase struct {
	name       string
	action     string
	existing   bool
	skip       bool
	fetchErr   error
	wantStatus int
	wantEnv    bool
}

var skipWithoutConfigCases = []skipWithoutConfigCase{
	{
		name:       "opened without config is ignored",
		action:     "open",
		skip:       true,
		fetchErr:   config.ErrConfigNotFound,
		wantStatus: http.StatusOK,
	},
	{
		name:       "wrapped not-found is ignored",
		action:     "open",
		skip:       true,
		fetchErr:   fmt.Errorf("fetching: %w", config.ErrConfigNotFound),
		wantStatus: http.StatusOK,
	},
	{
		name:       "update after config removal deletes the environment",
		action:     "update",
		existing:   true,
		skip:       true,
		fetchErr:   config.ErrConfigNotFound,
		wantStatus: http.StatusOK,
	},
	{
		name:       "fetch failure is not treated as missing config",
		action:     "open",
		skip:       true,
		fetchErr:   errFetchServerError,
		wantStatus: http.StatusInternalServerError,
	},
	{
		name:       "fetch failure keeps the existing environment",
		action:     "update",
		existing:   true,
		skip:       true,
		fetchErr:   errFetchServerError,
		wantStatus: http.StatusInternalServerError,
		wantEnv:    true,
	},
	{
		name:       "close without config or environment is a no-op",
		action:     "close",
		skip:       true,
		fetchErr:   config.ErrConfigNotFound,
		wantStatus: http.StatusOK,
	},
	{
		name:       "without skip the environment is still created",
		action:     "open",
		fetchErr:   config.ErrConfigNotFound,
		wantStatus: http.StatusOK,
		wantEnv:    true,
	},
}

func TestGitLabWebhookSkipWithoutConfig(t *testing.T) {
	const envName = "mr-0d7c4cc4-42"

	for _, tc := range skipWithoutConfigCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.existing {
				objs = append(objs, &divergeiov1alpha1.Environment{
					ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: "preview"},
				})
			}
			fakeK8s := newTestClient(objs...)
			fetcher := &staticConfigFetcher{err: tc.fetchErr}
			handler := &GitLabWebhookHandler{
				Client:            fakeK8s,
				Config:            WebhookConfig{SecretToken: "secret"},
				ConfigFetcher:     fetcher,
				DefaultNS:         "preview",
				SkipWithoutConfig: tc.skip,
			}

			body := fmt.Sprintf(`{
				"object_kind": "merge_request",
				"object_attributes": {
					"iid": 42,
					"action": %q,
					"source_branch": "feat/new-payments",
					"last_commit": {"id": "abc123def456789"}
				},
				"project": {"path_with_namespace": "team/payments-api"}
			}`, tc.action)
			req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			req.Header.Set("X-Gitlab-Token", "secret")

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			assertSkipOutcome(t, fakeK8s, envName, tc, rr.Code)
			if tc.action == "close" && fetcher.calls != 0 {
				t.Errorf("Expected no config fetch on close, got %d", fetcher.calls)
			}
		})
	}
}

func TestGitHubWebhookSkipWithoutConfig(t *testing.T) {
	const envName = "pr-661f4f5e-42"
	actions := map[string]string{"open": "opened", "update": "synchronize", "close": "closed"}

	for _, tc := range skipWithoutConfigCases {
		t.Run(tc.name, func(t *testing.T) {
			var objs []client.Object
			if tc.existing {
				objs = append(objs, &divergeiov1alpha1.Environment{
					ObjectMeta: metav1.ObjectMeta{Name: envName, Namespace: "preview"},
				})
			}
			fakeK8s := newTestClient(objs...)
			handler := &GitHubWebhookHandler{
				Client:            fakeK8s,
				Config:            WebhookConfig{SecretToken: "secret"},
				ConfigFetcher:     &staticConfigFetcher{err: tc.fetchErr},
				DefaultNS:         "preview",
				SkipWithoutConfig: tc.skip,
			}

			body := fmt.Sprintf(`{
				"action": %q,
				"pull_request": {
					"number": 42,
					"head": {"ref": "feat/new-payments", "sha": "abc123def456789"},
					"base": {"ref": "main"}
				},
				"repository": {"full_name": "divergedev/demo-payments-api"}
			}`, actions[tc.action])
			req, _ := http.NewRequest(http.MethodPost, "/", bytes.NewBufferString(body))
			req.Header.Set("X-Hub-Signature-256", generateSignature("secret", body))
			req.Header.Set("X-GitHub-Event", "pull_request")

			rr := httptest.NewRecorder()
			handler.ServeHTTP(rr, req)

			assertSkipOutcome(t, fakeK8s, envName, tc, rr.Code)
		})
	}
}

func assertSkipOutcome(t *testing.T, c client.Client, envName string, tc skipWithoutConfigCase, status int) {
	t.Helper()
	if status != tc.wantStatus {
		t.Fatalf("Expected status %d, got %d", tc.wantStatus, status)
	}
	err := c.Get(context.Background(), client.ObjectKey{Name: envName, Namespace: "preview"}, &divergeiov1alpha1.Environment{})
	switch {
	case tc.wantEnv && err != nil:
		t.Fatalf("Expected Environment to exist, got %v", err)
	case !tc.wantEnv && !apierrors.IsNotFound(err):
		t.Fatalf("Expected no Environment, got %v", err)
	}
}

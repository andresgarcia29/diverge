package cli

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
)

func newVerifyTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, divergeiov1alpha1.AddToScheme(scheme))
	return scheme
}

func TestVerifyCmd_HealthyEnvironmentAndHTTPPass(t *testing.T) {
	var gotHeader, gotBaggage string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotHeader = r.Header.Get("x-diverge-env")
		gotBaggage = r.Header.Get("baggage")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"status":"healthy","env":"pr-42"}`))
	}))
	t.Cleanup(srv.Close)

	env := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-42",
			Namespace: "default",
		},
		Spec: divergeiov1alpha1.EnvironmentSpec{
			Routing: divergeiov1alpha1.EnvironmentRouting{
				Mode:        "header",
				HeaderKey:   "x-diverge-env",
				HeaderValue: "pr-42",
			},
		},
		Status: divergeiov1alpha1.EnvironmentStatus{
			Phase: divergeiov1alpha1.PhaseRunning,
			URL:   srv.URL,
		},
	}

	healthyPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "orders-pr-42-abc",
			Namespace: "default",
			Labels: map[string]string{
				"diverge.io/environment": "pr-42",
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name:  "orders",
					Ready: true,
					State: corev1.ContainerState{
						Running: &corev1.ContainerStateRunning{},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newVerifyTestScheme(t)).
		WithObjects(env, healthyPod).
		Build()

	app := &App{Namespace: "default", Client: fakeClient}
	root := NewRootCmd(app)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"verify", "pr-42", "/healthz"})

	require.NoError(t, root.Execute())
	assert.Equal(t, "pr-42", gotHeader)
	assert.Contains(t, gotBaggage, "diverge.env=pr-42")

	output := out.String()
	assert.Contains(t, output, "workload-diagnostics")
	assert.Contains(t, output, "http-status:/healthz")
	assert.Contains(t, output, "[PASS]")
	assert.Contains(t, output, "Verification PASSED (2/2 checks)")
}

func TestVerifyCmd_FailsWhenDoctorDetectsCrashLoopBackOff(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	env := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-99",
			Namespace: "default",
		},
		Status: divergeiov1alpha1.EnvironmentStatus{
			Phase: divergeiov1alpha1.PhaseRunning,
			URL:   srv.URL,
		},
	}

	crashingPod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "api-pr-99-crash",
			Namespace: "default",
			Labels: map[string]string{
				"diverge.io/environment": "pr-99",
			},
		},
		Status: corev1.PodStatus{
			ContainerStatuses: []corev1.ContainerStatus{
				{
					Name: "api",
					State: corev1.ContainerState{
						Waiting: &corev1.ContainerStateWaiting{
							Reason:  "CrashLoopBackOff",
							Message: "back-off 10s restarting failed container=api",
						},
					},
				},
			},
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newVerifyTestScheme(t)).
		WithObjects(env, crashingPod).
		Build()

	app := &App{Namespace: "default", Client: fakeClient}
	root := NewRootCmd(app)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{"verify", "pr-99", "/healthz"})

	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification failed for environment pr-99")

	output := out.String()
	assert.Contains(t, output, "workload-diagnostics")
	assert.Contains(t, output, "UNHEALTHY")
	assert.Contains(t, output, "[FAIL]")
	assert.Contains(t, output, "Verification FAILED (1/2 checks failed)")
}

func TestVerifyCmd_MaxLatencySLO(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/slow" {
			time.Sleep(60 * time.Millisecond)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	t.Run("passes when within latency threshold", func(t *testing.T) {
		app := &App{}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/fast",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--max-latency", "2s",
		})

		require.NoError(t, root.Execute())
		assert.Contains(t, out.String(), "latency-slo:/fast")
		assert.Contains(t, out.String(), "Verification PASSED (2/2 checks)")
	})

	t.Run("fails when latency exceeds threshold", func(t *testing.T) {
		app := &App{}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/slow",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--max-latency", "10ms",
		})

		err := root.Execute()
		require.Error(t, err)
		assert.Contains(t, out.String(), "latency-slo:/slow")
		assert.Contains(t, out.String(), "[FAIL]")
		assert.Contains(t, out.String(), "Verification FAILED (1/2 checks failed)")
	})
}

func TestVerifyCmd_CompareBaselineStatusParity(t *testing.T) {
	t.Run("passes when baseline and preview status codes match", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ok"))
		}))
		t.Cleanup(srv.Close)

		app := &App{}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/api/orders",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--compare-baseline",
		})

		require.NoError(t, root.Execute())
		assert.Contains(t, out.String(), "baseline-parity:/api/orders")
		assert.Contains(t, out.String(), "PARITY")
		assert.Contains(t, out.String(), "Verification PASSED (2/2 checks)")
	})

	t.Run("fails when preview returns 500 and baseline returns 200", func(t *testing.T) {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.Header.Get("x-diverge-env") == "pr-42" {
				w.WriteHeader(http.StatusInternalServerError)
				_, _ = w.Write([]byte("internal error on preview"))
				return
			}
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("baseline ok"))
		}))
		t.Cleanup(srv.Close)

		app := &App{}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/api/orders",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--compare-baseline",
		})

		err := root.Execute()
		require.Error(t, err)
		output := out.String()
		assert.Contains(t, output, "baseline-parity:/api/orders")
		assert.Contains(t, output, "DIFF")
		assert.Contains(t, output, "baseline HTTP 200")
		assert.Contains(t, output, "preview HTTP 500")
		assert.Contains(t, output, "Verification FAILED (2/2 checks failed)")
	})
}

func TestVerifyCmd_BodyContainsAndJSONOutput(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"service":"checkout","featureFlag":"v2-enabled"}`))
	}))
	t.Cleanup(srv.Close)

	env := &divergeiov1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-77",
			Namespace: "staging",
		},
		Status: divergeiov1alpha1.EnvironmentStatus{
			Phase: divergeiov1alpha1.PhaseRunning,
			URL:   srv.URL,
		},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(newVerifyTestScheme(t)).
		WithObjects(env).
		Build()

	app := &App{Namespace: "staging", Client: fakeClient}
	root := NewRootCmd(app)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"verify", "pr-77",
		"--path", "/api/checkout",
		"--body-contains", `"featureFlag":"v2-enabled"`,
		"--json",
	})

	require.NoError(t, root.Execute())

	var report VerifyReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.Equal(t, "pr-77", report.Environment)
	assert.Equal(t, "staging", report.Namespace)
	assert.Equal(t, srv.URL, report.TargetURL)
	assert.True(t, report.Passed)
	require.NotNil(t, report.Doctor)
	assert.True(t, report.Doctor.Healthy)
	require.Len(t, report.Checks, 3)

	assert.Equal(t, "workload-diagnostics", report.Checks[0].Name)
	assert.True(t, report.Checks[0].Passed)
	assert.Equal(t, "http-status:/api/checkout", report.Checks[1].Name)
	assert.True(t, report.Checks[1].Passed)
	assert.Equal(t, "body-contains:/api/checkout", report.Checks[2].Name)
	assert.True(t, report.Checks[2].Passed)
	assert.Equal(t, "MATCH", report.Checks[2].Status)
}

func TestVerifyCmd_DefaultExpectStatusAllows2xxAnd3xx(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	app := &App{}
	root := NewRootCmd(app)
	var out bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&out)
	root.SetArgs([]string{
		"verify", "pr-42", "/readyz",
		"--kubeconfig", "/nonexistent/kubeconfig",
		"--base-url", srv.URL,
		"--skip-doctor",
	})

	require.NoError(t, root.Execute())
	assert.Contains(t, out.String(), "HTTP 204")
	assert.Contains(t, out.String(), "expected 2xx/3xx, got 204")
	assert.Contains(t, out.String(), "Verification PASSED (1/1 checks)")
}

func TestVerifyCmd_FailsWhenClusterUnavailableWithoutSkipDoctor(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	app := &App{}
	root := NewRootCmd(app)
	var out, errBuf bytes.Buffer
	root.SetOut(&out)
	root.SetErr(&errBuf)
	root.SetArgs([]string{
		"verify", "pr-42", "/healthz",
		"--kubeconfig", "/nonexistent/kubeconfig",
		"--base-url", srv.URL,
		"--json",
	})

	err := root.Execute()
	require.Error(t, err)
	assert.Contains(t, err.Error(), "verification failed for environment pr-42")

	var report VerifyReport
	require.NoError(t, json.Unmarshal(out.Bytes(), &report))
	assert.False(t, report.Passed)
	require.Len(t, report.Checks, 2)
	assert.Equal(t, "workload-diagnostics", report.Checks[0].Name)
	assert.False(t, report.Checks[0].Passed)
	assert.Equal(t, "ERROR", report.Checks[0].Status)
	assert.Contains(t, report.Checks[0].Message, "cluster unavailable")
}

func TestVerifyCmd_GitHubStepSummaryAndAnnotations(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/fail" {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("server error"))
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(srv.Close)

	t.Run("writes Markdown scorecard to GITHUB_STEP_SUMMARY file", func(t *testing.T) {
		tmpSummary := filepath.Join(t.TempDir(), "step-summary.md")
		t.Setenv("GITHUB_STEP_SUMMARY", tmpSummary)

		app := &App{Namespace: "staging"}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/healthz",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--github-summary",
		})

		require.NoError(t, root.Execute())

		summaryBytes, err := os.ReadFile(tmpSummary)
		require.NoError(t, err)
		md := string(summaryBytes)
		assert.Contains(t, md, "### ✅ Diverge Verification Passed: pr-42")
		assert.Contains(t, md, "**URL:** "+srv.URL+" | **Header:** `x-diverge-env: pr-42` | **Namespace:** `staging`")
		assert.Contains(t, md, "| Check | Status | Latency | Result | Details |")
		assert.Contains(t, md, "| :--- | :--- | :--- | :--- | :--- |")
		assert.Contains(t, md, "http-status:/healthz")
		assert.Contains(t, md, "✅ PASS")
	})

	t.Run("--github-summary renders Markdown table to stdout when GITHUB_STEP_SUMMARY is unset", func(t *testing.T) {
		t.Setenv("GITHUB_STEP_SUMMARY", "")

		app := &App{Namespace: "default"}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pr-42", "/healthz",
			"--kubeconfig", "/nonexistent/kubeconfig",
			"--base-url", srv.URL,
			"--skip-doctor",
			"--github-summary",
		})

		require.NoError(t, root.Execute())
		output := out.String()
		assert.Contains(t, output, "### ✅ Diverge Verification Passed: pr-42")
		assert.Contains(t, output, "| Check | Status | Latency | Result | Details |")
		assert.Contains(t, output, "✅ PASS")
	})

	t.Run("GITHUB_ACTIONS=true emits ::error annotations and doctor diagnostics on failure", func(t *testing.T) {
		tmpSummary := filepath.Join(t.TempDir(), "failed-summary.md")
		t.Setenv("GITHUB_STEP_SUMMARY", tmpSummary)
		t.Setenv("GITHUB_ACTIONS", "true")

		env := &divergeiov1alpha1.Environment{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pr-105",
				Namespace: "default",
			},
			Status: divergeiov1alpha1.EnvironmentStatus{
				Phase: divergeiov1alpha1.PhaseRunning,
				URL:   srv.URL,
			},
		}

		crashingPod := &corev1.Pod{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "api-pr-105-crash",
				Namespace: "default",
				Labels: map[string]string{
					"divergedev.com/environment": "pr-105",
				},
			},
			Status: corev1.PodStatus{
				ContainerStatuses: []corev1.ContainerStatus{
					{
						Name: "api",
						State: corev1.ContainerState{
							Waiting: &corev1.ContainerStateWaiting{
								Reason:  "CrashLoopBackOff",
								Message: "back-off 10s restarting failed container=api",
							},
						},
					},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(newVerifyTestScheme(t)).
			WithObjects(env, crashingPod).
			Build()

		app := &App{Namespace: "default", Client: fakeClient}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"verify", "pr-105", "/fail", "--github-summary"})

		err := root.Execute()
		require.Error(t, err)

		output := out.String()
		assert.Contains(t, output, "::error title=Diverge Verify (workload-diagnostics)::1 issue(s) detected")
		assert.Contains(t, output, "::error title=Diverge Verify (http-status%3A/fail)::expected 2xx/3xx, got 500")

		summaryBytes, err := os.ReadFile(tmpSummary)
		require.NoError(t, err)
		md := string(summaryBytes)
		assert.Contains(t, md, "### ❌ Diverge Verification Failed: pr-105")
		assert.Contains(t, md, "❌ FAIL")
		assert.Contains(t, md, "#### 🩺 Doctor Diagnostics")
		assert.Contains(t, md, "[DIV-105] Container/api (api-pr-105-crash): Container is repeatedly crashing (CrashLoopBackOff)")
		assert.Contains(t, md, "Remediation:")
	})
}

func TestVerifyCmd_PreviewGroupMultipleServices(t *testing.T) {
	t.Run("probes all service endpoints in PreviewGroup", func(t *testing.T) {
		var frontendHits, ordersHits int
		var frontendEnv, ordersEnv string
		var frontendBaggage, ordersBaggage string
		frontendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			frontendHits++
			frontendEnv = r.Header.Get("x-diverge-env")
			frontendBaggage = r.Header.Get("baggage")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("frontend ok"))
		}))
		t.Cleanup(frontendSrv.Close)

		ordersSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ordersHits++
			ordersEnv = r.Header.Get("x-diverge-env")
			ordersBaggage = r.Header.Get("baggage")
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("orders ok"))
		}))
		t.Cleanup(ordersSrv.Close)

		pg := &divergeiov1alpha1.PreviewGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pg-pr-42",
				Namespace: "default",
			},
			Spec: divergeiov1alpha1.PreviewGroupSpec{
				Routing: divergeiov1alpha1.PreviewGroupRouting{
					Mode:        "header",
					HeaderKey:   "x-diverge-env",
					HeaderValue: "pr-42",
				},
			},
			Status: divergeiov1alpha1.PreviewGroupStatus{
				Phase: divergeiov1alpha1.PreviewGroupPhaseRunning,
				Services: []divergeiov1alpha1.PreviewGroupServiceStatus{
					{Name: "frontend", EnvironmentName: "pg-pr-42-frontend", Phase: divergeiov1alpha1.PhaseRunning, URL: frontendSrv.URL},
					{Name: "orders-api", EnvironmentName: "pg-pr-42-orders", Phase: divergeiov1alpha1.PhaseRunning, URL: ordersSrv.URL},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(newVerifyTestScheme(t)).
			WithObjects(pg).
			Build()

		app := &App{Namespace: "default", Client: fakeClient}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"verify", "pg-pr-42", "/healthz", "--json"})

		require.NoError(t, root.Execute())
		assert.Equal(t, 1, frontendHits)
		assert.Equal(t, 1, ordersHits)
		assert.Equal(t, "pr-42", frontendEnv)
		assert.Equal(t, "pr-42", ordersEnv)
		assert.Equal(t, "diverge.env=pr-42", frontendBaggage)
		assert.Equal(t, "diverge.env=pr-42", ordersBaggage)

		var report VerifyReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		assert.True(t, report.Passed)
		assert.Equal(t, frontendSrv.URL+", "+ordersSrv.URL, report.TargetURL)
		require.Len(t, report.Checks, 3)
		assert.Equal(t, "workload-diagnostics", report.Checks[0].Name)
		assert.Equal(t, "http-status:frontend:/healthz", report.Checks[1].Name)
		assert.True(t, report.Checks[1].Passed)
		assert.Equal(t, "http-status:orders-api:/healthz", report.Checks[2].Name)
		assert.True(t, report.Checks[2].Passed)
	})

	t.Run("runs latency-slo, body-contains, and baseline-parity across all PreviewGroup services", func(t *testing.T) {
		frontendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready:true"))
		}))
		t.Cleanup(frontendSrv.Close)

		ordersSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("ready:true"))
		}))
		t.Cleanup(ordersSrv.Close)

		pg := &divergeiov1alpha1.PreviewGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pg-pr-88",
				Namespace: "default",
			},
			Spec: divergeiov1alpha1.PreviewGroupSpec{
				Routing: divergeiov1alpha1.PreviewGroupRouting{
					Mode:        "header",
					HeaderKey:   "x-diverge-env",
					HeaderValue: "pr-88",
				},
			},
			Status: divergeiov1alpha1.PreviewGroupStatus{
				Phase: divergeiov1alpha1.PreviewGroupPhaseRunning,
				Services: []divergeiov1alpha1.PreviewGroupServiceStatus{
					{Name: "frontend", EnvironmentName: "pg-pr-88-frontend", Phase: divergeiov1alpha1.PhaseRunning, URL: frontendSrv.URL},
					{Name: "orders-api", EnvironmentName: "pg-pr-88-orders", Phase: divergeiov1alpha1.PhaseRunning, URL: ordersSrv.URL},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(newVerifyTestScheme(t)).
			WithObjects(pg).
			Build()

		app := &App{Namespace: "default", Client: fakeClient}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{
			"verify", "pg-pr-88", "/healthz",
			"--max-latency", "2s",
			"--body-contains", "ready:true",
			"--compare-baseline",
			"--json",
		})

		require.NoError(t, root.Execute())

		var report VerifyReport
		require.NoError(t, json.Unmarshal(out.Bytes(), &report))
		assert.True(t, report.Passed)

		var checkNames []string
		for _, chk := range report.Checks {
			checkNames = append(checkNames, chk.Name)
		}
		assert.Contains(t, checkNames, "latency-slo:orders-api:/healthz")
		assert.Contains(t, checkNames, "body-contains:orders-api:/healthz")
		assert.Contains(t, checkNames, "baseline-parity:orders-api:/healthz")
	})

	t.Run("fails when a secondary service in PreviewGroup returns 500", func(t *testing.T) {
		frontendSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
			_, _ = w.Write([]byte("frontend ok"))
		}))
		t.Cleanup(frontendSrv.Close)

		ordersSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusInternalServerError)
			_, _ = w.Write([]byte("orders error"))
		}))
		t.Cleanup(ordersSrv.Close)

		pg := &divergeiov1alpha1.PreviewGroup{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "pg-pr-99",
				Namespace: "default",
			},
			Status: divergeiov1alpha1.PreviewGroupStatus{
				Phase: divergeiov1alpha1.PreviewGroupPhaseRunning,
				Services: []divergeiov1alpha1.PreviewGroupServiceStatus{
					{Name: "frontend", EnvironmentName: "pg-pr-99-frontend", Phase: divergeiov1alpha1.PhaseRunning, URL: frontendSrv.URL},
					{Name: "orders-api", EnvironmentName: "pg-pr-99-orders", Phase: divergeiov1alpha1.PhaseRunning, URL: ordersSrv.URL},
				},
			},
		}

		fakeClient := fake.NewClientBuilder().
			WithScheme(newVerifyTestScheme(t)).
			WithObjects(pg).
			Build()

		app := &App{Namespace: "default", Client: fakeClient}
		root := NewRootCmd(app)
		var out bytes.Buffer
		root.SetOut(&out)
		root.SetErr(&out)
		root.SetArgs([]string{"verify", "pg-pr-99", "/healthz"})

		err := root.Execute()
		require.Error(t, err)
		output := out.String()
		assert.Contains(t, output, "http-status:frontend:/healthz")
		assert.Contains(t, output, "http-status:orders-api:/healthz")
		assert.Contains(t, output, "Verification FAILED (1/3 checks failed)")
	})
}

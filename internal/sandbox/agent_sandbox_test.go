//go:build !no_sandbox

package sandbox

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/registry"
	pkgsandbox "github.com/divergedev/diverge/pkg/sandbox"
)

func TestRegistryHasProviders(t *testing.T) {
	list := pkgsandbox.Providers.List()
	assert.Contains(t, list, "noop")
	assert.Contains(t, list, "none")
	assert.Contains(t, list, "agent-sandbox")
	assert.Contains(t, list, "pod")
	assert.Contains(t, list, "k8s-pod")

	p, err := pkgsandbox.Providers.Create("noop", registry.Deps{})
	require.NoError(t, err)
	assert.NotNil(t, p)

	podP, err := pkgsandbox.Providers.Create("pod", registry.Deps{})
	require.NoError(t, err)
	assert.NotNil(t, podP)
}

func TestAgentSandboxProvider(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewAgentSandboxProvider(fakeClient, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-abc",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Add payment gateway",
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/org/payments",
			},
			Sandbox: v1alpha1.AgentTaskSandbox{
				PoolRef: "custom-pool",
			},
		},
	}

	res, err := p.Provision(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "sb-claim-task-abc", res.ClaimName)

	// In fake client without Agent Sandbox controller running, claim has no status yet
	status, err := p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Pending", status.Phase)
	assert.False(t, status.Ready)

	err = p.Teardown(ctx, task)
	assert.NoError(t, err)
}

func TestBuildSandboxClaimSpec(t *testing.T) {
	// 1. WarmPoolRef
	taskPool := &v1alpha1.AgentTask{
		Spec: v1alpha1.AgentTaskSpec{
			Sandbox: v1alpha1.AgentTaskSandbox{
				PoolRef: "prewarmed-dev",
			},
		},
	}
	specPool := BuildSandboxClaimSpec(taskPool)
	assert.Equal(t, map[string]interface{}{"name": "prewarmed-dev"}, specPool["warmPoolRef"])

	// 2. TemplateRef
	taskTmpl := &v1alpha1.AgentTask{
		Spec: v1alpha1.AgentTaskSpec{
			Sandbox: v1alpha1.AgentTaskSandbox{
				TemplateRef: "heavy-compute",
			},
		},
	}
	specTmpl := BuildSandboxClaimSpec(taskTmpl)
	assert.Equal(t, map[string]interface{}{"name": "heavy-compute"}, specTmpl["sandboxTemplateRef"])

	// 3. Fallback default template with resource limits, NetworkPolicy labels, and task env vars
	taskDefault := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-default-1",
			Namespace: "staging",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:     "Fix login regression",
			BudgetUSD:     "8.00",
			MaxIterations: 12,
			DraftPR:       true,
			Repository: v1alpha1.AgentTaskRepository{
				URL:           "https://github.com/org/app",
				BaseBranch:    "develop",
				WorkingBranch: "agent/fix-login",
			},
		},
	}
	specDefault := BuildSandboxClaimSpec(taskDefault)
	assert.Nil(t, specDefault["warmPoolRef"])
	assert.Nil(t, specDefault["sandboxTemplateRef"])

	tmpl, ok := specDefault["template"].(map[string]interface{})
	require.True(t, ok)

	// Verify NetworkPolicy pod selector label is present on template.metadata.labels
	tmplMeta, ok := tmpl["metadata"].(map[string]interface{})
	require.True(t, ok)
	tmplLabels, ok := tmplMeta["labels"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, "task-default-1", tmplLabels["divergedev.com/agent-task"])
	assert.Equal(t, "diverge", tmplLabels["app.kubernetes.io/managed-by"])

	tmplSpec, ok := tmpl["spec"].(map[string]interface{})
	require.True(t, ok)
	containers, ok := tmplSpec["containers"].([]interface{})
	require.True(t, ok)
	require.Len(t, containers, 1)

	container := containers[0].(map[string]interface{})
	assert.Equal(t, "agent", container["name"])
	assert.Equal(t, DefaultAgentImage, container["image"])
	res, ok := container["resources"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, map[string]interface{}{"cpu": "500m", "memory": "512Mi"}, res["requests"])
	assert.Equal(t, map[string]interface{}{"cpu": "2", "memory": "2Gi"}, res["limits"])

	// Verify injected task context environment variables
	envList, ok := container["env"].([]interface{})
	require.True(t, ok)
	envMap := make(map[string]string, len(envList))
	for _, e := range envList {
		em := e.(map[string]interface{})
		envMap[em["name"].(string)] = em["value"].(string)
	}
	assert.Equal(t, "task-default-1", envMap["DIVERGE_TASK_NAME"])
	assert.Equal(t, "staging", envMap["DIVERGE_TASK_NAMESPACE"])
	assert.Equal(t, "Fix login regression", envMap["DIVERGE_OBJECTIVE"])
	assert.Equal(t, "https://github.com/org/app", envMap["DIVERGE_REPO_URL"])
	assert.Equal(t, "develop", envMap["DIVERGE_BASE_BRANCH"])
	assert.Equal(t, "agent/fix-login", envMap["DIVERGE_WORKING_BRANCH"])
	assert.Equal(t, "true", envMap["DIVERGE_DRAFT_PR"])
	assert.Equal(t, "8.00", envMap["DIVERGE_BUDGET_USD"])
	assert.Equal(t, "12", envMap["DIVERGE_MAX_ITERATIONS"])
	assert.Equal(t, "/etc/diverge/token/token", envMap["DIVERGE_TOKEN_FILE"])
	assert.Contains(t, envMap["DIVERGE_MCP_URL"], "/mcp")
	assert.Contains(t, envMap["DIVERGE_METER_URL"], "/api/v1/agenttask/meter")

	// Assert securityContext & volumes
	assert.Equal(t, false, tmplSpec["automountServiceAccountToken"])
	secCtx, ok := tmplSpec["securityContext"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, true, secCtx["runAsNonRoot"])
	assert.Equal(t, int64(10001), secCtx["runAsUser"])
	assert.Equal(t, map[string]interface{}{"type": "RuntimeDefault"}, secCtx["seccompProfile"])

	cSecCtx, ok := container["securityContext"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, false, cSecCtx["allowPrivilegeEscalation"])
	assert.Equal(t, true, cSecCtx["readOnlyRootFilesystem"])
	caps, ok := cSecCtx["capabilities"].(map[string]interface{})
	require.True(t, ok)
	assert.Equal(t, []interface{}{"ALL"}, caps["drop"])

	volumes, ok := tmplSpec["volumes"].([]interface{})
	require.True(t, ok)
	assert.Len(t, volumes, 3)

	// 4. Long task name (>63 chars) must produce a valid <=63-char label value
	longName := "agent-task-with-very-long-name-exceeding-sixty-three-characters-limit-1234567890"
	require.Greater(t, len(longName), 63)
	taskLong := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      longName,
			Namespace: "staging",
		},
	}
	specLong := BuildSandboxClaimSpec(taskLong)
	tmplLong := specLong["template"].(map[string]interface{})
	metaLong := tmplLong["metadata"].(map[string]interface{})
	labelsLong := metaLong["labels"].(map[string]interface{})
	labelVal := labelsLong["divergedev.com/agent-task"].(string)
	assert.Equal(t, 63, len(labelVal))

	// Verify DraftPR=false sets DIVERGE_DRAFT_PR="false"
	longEnvList := tmplLong["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"].([]interface{})
	longEnvMap := make(map[string]string, len(longEnvList))
	for _, e := range longEnvList {
		em := e.(map[string]interface{})
		if val, ok := em["value"].(string); ok {
			longEnvMap[em["name"].(string)] = val
		}
	}
	assert.Equal(t, "false", longEnvMap["DIVERGE_DRAFT_PR"])

	// 5. Verify divergedev.com/scm-secret injects DIVERGE_SCM_TOKEN only for allowed diverge-scm-* Secret names
	taskSCM := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-scm",
			Namespace: "staging",
			Annotations: map[string]string{
				"divergedev.com/scm-secret": "diverge-scm-token",
			},
		},
	}
	specSCM := BuildSandboxClaimSpec(taskSCM)
	scmEnvList := specSCM["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"].([]interface{})
	var foundSCMEnv map[string]interface{}
	for _, e := range scmEnvList {
		em := e.(map[string]interface{})
		if em["name"] == "DIVERGE_SCM_TOKEN" {
			foundSCMEnv = em
			break
		}
	}
	require.NotNil(t, foundSCMEnv)
	vf := foundSCMEnv["valueFrom"].(map[string]interface{})["secretKeyRef"].(map[string]interface{})
	assert.Equal(t, "diverge-scm-token", vf["name"])
	assert.Equal(t, "token", vf["key"])
	assert.Equal(t, true, vf["optional"])

	taskBadSCM := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-bad-scm",
			Namespace: "staging",
			Annotations: map[string]string{
				"divergedev.com/scm-secret": "arbitrary-secret",
			},
		},
	}
	specBadSCM := BuildSandboxClaimSpec(taskBadSCM)
	badEnvList := specBadSCM["template"].(map[string]interface{})["spec"].(map[string]interface{})["containers"].([]interface{})[0].(map[string]interface{})["env"].([]interface{})
	for _, e := range badEnvList {
		em := e.(map[string]interface{})
		assert.NotEqual(t, "DIVERGE_SCM_TOKEN", em["name"])
	}
}

func TestAgentSandboxProvider_SuspendAndResume(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	_ = v1alpha1.AddToScheme(scheme)

	fakeClient := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewAgentSandboxProvider(fakeClient, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-suspend",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Test suspend and resume",
		},
	}

	res, err := p.Provision(ctx, task)
	require.NoError(t, err)
	task.Status.SandboxClaimRef = res.ClaimName

	require.NoError(t, p.Suspend(ctx, task))

	claim := &unstructured.Unstructured{}
	claim.SetGroupVersionKind(sandboxClaimGVK)
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: res.ClaimName}, claim))
	mode, _, _ := unstructured.NestedString(claim.Object, "spec", "operatingMode")
	assert.Equal(t, "Suspended", mode)

	require.NoError(t, p.Resume(ctx, task))
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: res.ClaimName}, claim))
	mode, _, _ = unstructured.NestedString(claim.Object, "spec", "operatingMode")
	assert.Equal(t, "Running", mode)
}

func TestAgentSandboxProvider_FallbackToPodOnNoMatchError(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))

	noMatchErr := &meta.NoKindMatchError{
		GroupKind:        schema.GroupKind{Group: "extensions.agents.x-k8s.io", Kind: "SandboxClaim"},
		SearchedVersions: []string{"v1beta1"},
	}

	fakeClient := fake.NewClientBuilder().
		WithScheme(scheme).
		WithInterceptorFuncs(interceptor.Funcs{
			Create: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.CreateOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == sandboxClaimGVK {
					return noMatchErr
				}
				return c.Create(ctx, obj, opts...)
			},
			Get: func(ctx context.Context, c client.WithWatch, key client.ObjectKey, obj client.Object, opts ...client.GetOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == sandboxClaimGVK {
					return noMatchErr
				}
				return c.Get(ctx, key, obj, opts...)
			},
			Delete: func(ctx context.Context, c client.WithWatch, obj client.Object, opts ...client.DeleteOption) error {
				if u, ok := obj.(*unstructured.Unstructured); ok && u.GroupVersionKind() == sandboxClaimGVK {
					return noMatchErr
				}
				return c.Delete(ctx, obj, opts...)
			},
		}).
		Build()

	p := NewAgentSandboxProvider(fakeClient, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fallback-task",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective: "Verify automatic fallback to native PodSandboxProvider",
			Repository: v1alpha1.AgentTaskRepository{
				URL: "https://github.com/org/repo.git",
			},
		},
	}

	res, err := p.Provision(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "sb-pod-fallback-task", res.ClaimName)
	assert.Equal(t, "sb-pod-fallback-task", res.PodName)

	// Verify Pod was created in the cluster
	pod := &corev1.Pod{}
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-pod-fallback-task"}, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.0.0.42"
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	require.NoError(t, fakeClient.Status().Update(ctx, pod))

	status, err := p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Running", status.Phase)
	assert.True(t, status.Ready)
	assert.Equal(t, "10.0.0.42", status.PodIP)

	require.NoError(t, p.Teardown(ctx, task))
	err = fakeClient.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-pod-fallback-task"}, pod)
	assert.True(t, apierrors.IsNotFound(err))
}

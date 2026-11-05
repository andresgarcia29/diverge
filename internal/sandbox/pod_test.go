//go:build !no_sandbox

package sandbox

import (
	"context"
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	fakek8s "k8s.io/client-go/kubernetes/fake"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/divergedev/diverge/api/v1alpha1"
	pkgauth "github.com/divergedev/diverge/pkg/auth"
	pkgsandbox "github.com/divergedev/diverge/pkg/sandbox"
)

func buildPodTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	require.NoError(t, corev1.AddToScheme(scheme))
	require.NoError(t, v1alpha1.AddToScheme(scheme))
	return scheme
}

func TestPodSandboxProvider_ProvisionAndSpec(t *testing.T) {
	ctx := context.Background()
	scheme := buildPodTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodSandboxProvider(c, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "fix-checkout",
			Namespace: "staging",
			UID:       "uid-1234",
			Annotations: map[string]string{
				"divergedev.com/runtime-class": "gvisor",
				"divergedev.com/sandbox-image": "ghcr.io/divergedev/custom-agent:v1",
			},
		},
		Spec: v1alpha1.AgentTaskSpec{
			Objective:     "Fix race condition in checkout handler",
			BudgetUSD:     "12.50",
			MaxIterations: 7,
			DraftPR:       true,
			Repository: v1alpha1.AgentTaskRepository{
				URL:           "https://github.com/example/shop.git",
				BaseBranch:    "",
				WorkingBranch: "agent/fix-checkout",
			},
			Sandbox: v1alpha1.AgentTaskSandbox{
				Provider:       "pod",
				TimeoutSeconds: 0, // should default to 3600
			},
		},
	}

	res, err := p.Provision(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "sb-pod-fix-checkout", res.ClaimName)
	assert.Equal(t, "sb-pod-fix-checkout", res.PodName)
	assert.Equal(t, "/etc/diverge/token/token", res.TokenMountPath)
	assert.False(t, res.Ready)

	// Calling Provision again should be idempotent (ignores AlreadyExists)
	res2, err := p.Provision(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, res.PodName, res2.PodName)

	// Verify Pod spec in cluster
	pod := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "staging", Name: "sb-pod-fix-checkout"}, pod))

	// Labels & OwnerReference
	assert.Equal(t, "fix-checkout", pod.Labels["divergedev.com/agent-task"])
	assert.Equal(t, "diverge", pod.Labels["app.kubernetes.io/managed-by"])
	assert.Equal(t, "pod", pod.Labels["divergedev.com/sandbox-provider"])
	require.Len(t, pod.OwnerReferences, 1)
	assert.Equal(t, "fix-checkout", pod.OwnerReferences[0].Name)

	// Pod-level hardening
	require.NotNil(t, pod.Spec.AutomountServiceAccountToken)
	assert.False(t, *pod.Spec.AutomountServiceAccountToken)
	assert.Equal(t, corev1.RestartPolicyNever, pod.Spec.RestartPolicy)
	require.NotNil(t, pod.Spec.ActiveDeadlineSeconds)
	assert.Equal(t, int64(3600), *pod.Spec.ActiveDeadlineSeconds)
	require.NotNil(t, pod.Spec.RuntimeClassName)
	assert.Equal(t, "gvisor", *pod.Spec.RuntimeClassName)

	require.NotNil(t, pod.Spec.SecurityContext)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsNonRoot)
	assert.True(t, *pod.Spec.SecurityContext.RunAsNonRoot)
	require.NotNil(t, pod.Spec.SecurityContext.RunAsUser)
	assert.Equal(t, int64(10001), *pod.Spec.SecurityContext.RunAsUser)
	require.NotNil(t, pod.Spec.SecurityContext.SeccompProfile)
	assert.Equal(t, corev1.SeccompProfileTypeRuntimeDefault, pod.Spec.SecurityContext.SeccompProfile.Type)

	// Container spec
	require.Len(t, pod.Spec.Containers, 1)
	container := pod.Spec.Containers[0]
	assert.Equal(t, "agent", container.Name)
	assert.Equal(t, "ghcr.io/divergedev/custom-agent:v1", container.Image)

	// Env vars
	envMap := make(map[string]string, len(container.Env))
	for _, e := range container.Env {
		envMap[e.Name] = e.Value
	}
	assert.Equal(t, "fix-checkout", envMap["DIVERGE_TASK_NAME"])
	assert.Equal(t, "staging", envMap["DIVERGE_TASK_NAMESPACE"])
	assert.Equal(t, "Fix race condition in checkout handler", envMap["DIVERGE_OBJECTIVE"])
	assert.Equal(t, "https://github.com/example/shop.git", envMap["DIVERGE_REPO_URL"])
	assert.Equal(t, "main", envMap["DIVERGE_BASE_BRANCH"])
	assert.Equal(t, "agent/fix-checkout", envMap["DIVERGE_WORKING_BRANCH"])
	assert.Equal(t, "true", envMap["DIVERGE_DRAFT_PR"])
	assert.Equal(t, "12.50", envMap["DIVERGE_BUDGET_USD"])
	assert.Equal(t, "7", envMap["DIVERGE_MAX_ITERATIONS"])
	assert.Equal(t, "/etc/diverge/token/token", envMap["DIVERGE_TOKEN_FILE"])
	assert.Equal(t, "http://diverge-server.diverge-system.svc:8080/mcp", envMap["DIVERGE_MCP_URL"])
	assert.Equal(t, "http://diverge-server.diverge-system.svc:8080/api/v1/agenttask/meter", envMap["DIVERGE_METER_URL"])

	// Resources
	assert.Equal(t, resource.MustParse("500m"), container.Resources.Requests[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("512Mi"), container.Resources.Requests[corev1.ResourceMemory])
	assert.Equal(t, resource.MustParse("2"), container.Resources.Limits[corev1.ResourceCPU])
	assert.Equal(t, resource.MustParse("2Gi"), container.Resources.Limits[corev1.ResourceMemory])

	// Container SecurityContext
	require.NotNil(t, container.SecurityContext)
	require.NotNil(t, container.SecurityContext.AllowPrivilegeEscalation)
	assert.False(t, *container.SecurityContext.AllowPrivilegeEscalation)
	require.NotNil(t, container.SecurityContext.ReadOnlyRootFilesystem)
	assert.True(t, *container.SecurityContext.ReadOnlyRootFilesystem)
	require.NotNil(t, container.SecurityContext.Capabilities)
	assert.Equal(t, []corev1.Capability{"ALL"}, container.SecurityContext.Capabilities.Drop)

	// VolumeMounts & Volumes
	require.Len(t, container.VolumeMounts, 3)
	assert.Equal(t, corev1.VolumeMount{Name: "workspace", MountPath: "/workspace"}, container.VolumeMounts[0])
	assert.Equal(t, corev1.VolumeMount{Name: "tmp", MountPath: "/tmp"}, container.VolumeMounts[1])
	assert.Equal(t, corev1.VolumeMount{Name: "token", MountPath: "/etc/diverge/token", ReadOnly: true}, container.VolumeMounts[2])

	require.Len(t, pod.Spec.Volumes, 3)
	assert.Equal(t, "workspace", pod.Spec.Volumes[0].Name)
	require.NotNil(t, pod.Spec.Volumes[0].EmptyDir)
	require.NotNil(t, pod.Spec.Volumes[0].EmptyDir.SizeLimit)
	assert.Equal(t, resource.MustParse("10Gi"), *pod.Spec.Volumes[0].EmptyDir.SizeLimit)

	assert.Equal(t, "tmp", pod.Spec.Volumes[1].Name)
	require.NotNil(t, pod.Spec.Volumes[1].EmptyDir)
	require.NotNil(t, pod.Spec.Volumes[1].EmptyDir.SizeLimit)
	assert.Equal(t, resource.MustParse("1Gi"), *pod.Spec.Volumes[1].EmptyDir.SizeLimit)

	assert.Equal(t, "token", pod.Spec.Volumes[2].Name)
	require.NotNil(t, pod.Spec.Volumes[2].Secret)
	assert.Equal(t, pkgauth.SecretName("fix-checkout"), pod.Spec.Volumes[2].Secret.SecretName)
	require.NotNil(t, pod.Spec.Volumes[2].Secret.DefaultMode)
	assert.Equal(t, int32(0400), *pod.Spec.Volumes[2].Secret.DefaultMode)
	require.NotNil(t, pod.Spec.Volumes[2].Secret.Optional)
	assert.True(t, *pod.Spec.Volumes[2].Secret.Optional)

	// Verify DraftPR=false propagates DIVERGE_DRAFT_PR="false" and allowed scm-secret injects DIVERGE_SCM_TOKEN
	taskNoDraft := task.DeepCopy()
	taskNoDraft.Name = "fix-checkout-ready"
	taskNoDraft.Spec.DraftPR = false
	taskNoDraft.Annotations["divergedev.com/scm-secret"] = "diverge-scm-token"
	_, err = p.Provision(ctx, taskNoDraft)
	require.NoError(t, err)
	podNoDraft := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "staging", Name: "sb-pod-fix-checkout-ready"}, podNoDraft))
	envNoDraft := make(map[string]string, len(podNoDraft.Spec.Containers[0].Env))
	var scmEnvVar *corev1.EnvVar
	for i := range podNoDraft.Spec.Containers[0].Env {
		e := &podNoDraft.Spec.Containers[0].Env[i]
		envNoDraft[e.Name] = e.Value
		if e.Name == "DIVERGE_SCM_TOKEN" {
			scmEnvVar = e
		}
	}
	assert.Equal(t, "false", envNoDraft["DIVERGE_DRAFT_PR"])
	require.NotNil(t, scmEnvVar)
	require.NotNil(t, scmEnvVar.ValueFrom)
	require.NotNil(t, scmEnvVar.ValueFrom.SecretKeyRef)
	assert.Equal(t, "diverge-scm-token", scmEnvVar.ValueFrom.SecretKeyRef.Name)
	assert.Equal(t, "token", scmEnvVar.ValueFrom.SecretKeyRef.Key)
	require.NotNil(t, scmEnvVar.ValueFrom.SecretKeyRef.Optional)
	assert.True(t, *scmEnvVar.ValueFrom.SecretKeyRef.Optional)

	// Arbitrary secret names must be rejected and produce no DIVERGE_SCM_TOKEN entry
	taskBadSecret := task.DeepCopy()
	taskBadSecret.Name = "fix-checkout-bad-secret"
	taskBadSecret.Annotations["divergedev.com/scm-secret"] = "arbitrary-namespace-secret"
	_, err = p.Provision(ctx, taskBadSecret)
	require.NoError(t, err)
	podBadSecret := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "staging", Name: "sb-pod-fix-checkout-bad-secret"}, podBadSecret))
	for _, e := range podBadSecret.Spec.Containers[0].Env {
		assert.NotEqual(t, "DIVERGE_SCM_TOKEN", e.Name)
	}
}

func TestPodSandboxProvider_StatusTransitionsAndTeardown(t *testing.T) {
	ctx := context.Background()
	scheme := buildPodTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodSandboxProvider(c, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-lifecycle",
			Namespace: "default",
		},
		Spec: v1alpha1.AgentTaskSpec{
			Sandbox: v1alpha1.AgentTaskSandbox{
				TimeoutSeconds: 1800,
			},
		},
	}

	// 1. Before Provision: Status returns Pending / not found
	st, err := p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Pending", st.Phase)
	assert.False(t, st.Ready)
	assert.Equal(t, "Sandbox pod not found", st.Message)

	// 2. Provision Pod -> Pending initially
	_, err = p.Provision(ctx, task)
	require.NoError(t, err)

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Pending", st.Phase)
	assert.False(t, st.Ready)

	// 3. Transition Pod to Running without conditions -> Ready=false until PodReady=True
	pod := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-pod-task-lifecycle"}, pod))
	pod.Status.Phase = corev1.PodRunning
	pod.Status.PodIP = "10.244.0.15"
	require.NoError(t, c.Status().Update(ctx, pod))

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Running", st.Phase)
	assert.False(t, st.Ready)
	assert.Equal(t, "10.244.0.15", st.PodIP)

	// 4. Running with PodReady=False -> Ready=false
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionFalse},
	}
	require.NoError(t, c.Status().Update(ctx, pod))

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Running", st.Phase)
	assert.False(t, st.Ready)

	// 5. Running with PodReady=True -> Ready=true
	pod.Status.Conditions = []corev1.PodCondition{
		{Type: corev1.PodReady, Status: corev1.ConditionTrue},
	}
	require.NoError(t, c.Status().Update(ctx, pod))

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Running", st.Phase)
	assert.True(t, st.Ready)

	// 6. PodFailed -> Failed, Ready=false
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, c.Status().Update(ctx, pod))

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Failed", st.Phase)
	assert.False(t, st.Ready)

	// 7. PodSucceeded -> Succeeded, Ready=false
	pod.Status.Phase = corev1.PodSucceeded
	require.NoError(t, c.Status().Update(ctx, pod))

	st, err = p.Status(ctx, task)
	require.NoError(t, err)
	assert.Equal(t, "Succeeded", st.Phase)
	assert.False(t, st.Ready)

	// 8. Teardown deletes Pod and is idempotent on repeat calls
	require.NoError(t, p.Teardown(ctx, task))
	err = c.Get(ctx, client.ObjectKey{Namespace: "default", Name: "sb-pod-task-lifecycle"}, pod)
	assert.True(t, apierrors.IsNotFound(err))

	// Second Teardown call must succeed (idempotent)
	require.NoError(t, p.Teardown(ctx, task))
}

func TestPodSandboxProvider_StreamLogs(t *testing.T) {
	ctx := context.Background()
	scheme := buildPodTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-logs",
			Namespace: "default",
		},
	}

	// 1. Nil clientset fallback
	pNil := NewPodSandboxProvider(c, nil, scheme)
	rc, err := pNil.StreamLogs(ctx, task, pkgsandbox.LogOptions{})
	require.NoError(t, err)
	data, err := io.ReadAll(rc)
	require.NoError(t, err)
	require.NoError(t, rc.Close())
	assert.Contains(t, string(data), "clientset not available")

	// 2. Fake clientset log streaming
	cs := fakek8s.NewSimpleClientset(&corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      SandboxPodName(task.Name),
			Namespace: "default",
		},
	})
	pWithCS := NewPodSandboxProvider(c, cs, scheme)
	tail := int64(50)
	rc2, err := pWithCS.StreamLogs(ctx, task, pkgsandbox.LogOptions{
		Follow:     true,
		Timestamps: true,
		TailLines:  &tail,
	})
	require.NoError(t, err)
	_, err = io.ReadAll(rc2)
	require.NoError(t, err)
	require.NoError(t, rc2.Close())
}

func TestSandboxPodName_LongNameTruncation(t *testing.T) {
	short := SandboxPodName("my-task")
	assert.Equal(t, "sb-pod-my-task", short)

	longTaskName := "autonomous-agent-task-with-an-extremely-long-identifier-that-exceeds-dns1123-limit"
	require.Greater(t, len("sb-pod-"+longTaskName), 63)

	podName1 := SandboxPodName(longTaskName)
	podName2 := SandboxPodName(longTaskName)
	assert.Equal(t, podName1, podName2, "SandboxPodName must be deterministic")
	assert.LessOrEqual(t, len(podName1), 63, "SandboxPodName must be <= 63 chars")
	assert.True(t, strings.HasPrefix(podName1, "sb-pod-"))

	differentLong := longTaskName + "-other"
	assert.NotEqual(t, podName1, SandboxPodName(differentLong), "Distinct long task names must not collide")
}

func TestPodSandboxProvider_SuspendAndResume(t *testing.T) {
	ctx := context.Background()
	scheme := buildPodTestScheme(t)
	c := fake.NewClientBuilder().WithScheme(scheme).Build()
	p := NewPodSandboxProvider(c, nil, scheme)

	task := &v1alpha1.AgentTask{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "task-suspend",
			Namespace: "default",
		},
	}

	_, err := p.Provision(ctx, task)
	require.NoError(t, err)

	podKey := client.ObjectKey{Namespace: "default", Name: SandboxPodName(task.Name)}
	pod := &corev1.Pod{}
	require.NoError(t, c.Get(ctx, podKey, pod))

	// Suspend deletes the pod to reclaim compute
	require.NoError(t, p.Suspend(ctx, task))
	err = c.Get(ctx, podKey, pod)
	assert.True(t, apierrors.IsNotFound(err))

	// Resume re-provisions the pod when missing
	require.NoError(t, p.Resume(ctx, task))
	require.NoError(t, c.Get(ctx, podKey, pod))

	// If the pod is in a terminal phase (PodFailed), Resume deletes and re-provisions it
	pod.Status.Phase = corev1.PodFailed
	require.NoError(t, c.Status().Update(ctx, pod))
	require.NoError(t, p.Resume(ctx, task))
	require.NoError(t, c.Get(ctx, podKey, pod))
	assert.NotEqual(t, corev1.PodFailed, pod.Status.Phase)
}

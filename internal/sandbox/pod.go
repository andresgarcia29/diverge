//go:build !no_sandbox

package sandbox

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"strconv"
	"strings"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	"github.com/divergedev/diverge/api/v1alpha1"
	pkgauth "github.com/divergedev/diverge/pkg/auth"
	pkgsandbox "github.com/divergedev/diverge/pkg/sandbox"
)

// PodSandboxProvider provisions standalone hardened Kubernetes Pods for AgentTask sandboxes.
type PodSandboxProvider struct {
	client    client.Client
	clientset kubernetes.Interface
	scheme    *runtime.Scheme
}

var (
	_ pkgsandbox.SandboxProvider  = (*PodSandboxProvider)(nil)
	_ pkgsandbox.SandboxSuspender = (*PodSandboxProvider)(nil)
)

// NewPodSandboxProvider initializes a new PodSandboxProvider.
func NewPodSandboxProvider(c client.Client, cs kubernetes.Interface, scheme *runtime.Scheme) *PodSandboxProvider {
	return &PodSandboxProvider{
		client:    c,
		clientset: cs,
		scheme:    scheme,
	}
}

// SandboxPodName returns a valid DNS-1123 pod name (<= 63 chars) for an AgentTask.
func SandboxPodName(taskName string) string {
	name := fmt.Sprintf("sb-pod-%s", strings.ToLower(taskName))
	if len(name) <= 63 {
		return name
	}
	h := sha256.Sum256([]byte(taskName))
	suffix := hex.EncodeToString(h[:4]) // 8 hex chars
	maxPrefix := 63 - 1 - len(suffix)
	prefix := strings.TrimRight(name[:maxPrefix], "-.")
	return fmt.Sprintf("%s-%s", prefix, suffix)
}

func ptr[T any](v T) *T {
	return &v
}

// Provision creates a hardened Kubernetes Pod for the AgentTask.
func (p *PodSandboxProvider) Provision(ctx context.Context, task *v1alpha1.AgentTask) (*pkgsandbox.SandboxResult, error) {
	podName := SandboxPodName(task.Name)

	baseBranch := task.Spec.Repository.BaseBranch
	if baseBranch == "" {
		baseBranch = "main"
	}

	timeoutSec := int64(task.Spec.Sandbox.TimeoutSeconds)
	if timeoutSec <= 0 {
		timeoutSec = 3600
	}

	image := DefaultAgentImage
	if customImg := task.Annotations["divergedev.com/sandbox-image"]; customImg != "" {
		image = customImg
	}

	var runtimeClassName *string
	if rc := task.Annotations["divergedev.com/runtime-class"]; rc != "" {
		runtimeClassName = ptr(rc)
	}

	serverBaseURL := resolveServerBaseURL()
	containerEnv := []corev1.EnvVar{
		{Name: "DIVERGE_TASK_NAME", Value: task.Name},
		{Name: "DIVERGE_TASK_NAMESPACE", Value: task.Namespace},
		{Name: "DIVERGE_OBJECTIVE", Value: task.Spec.Objective},
		{Name: "DIVERGE_REPO_URL", Value: task.Spec.Repository.URL},
		{Name: "DIVERGE_BASE_BRANCH", Value: baseBranch},
		{Name: "DIVERGE_WORKING_BRANCH", Value: task.Spec.Repository.WorkingBranch},
		{Name: "DIVERGE_DRAFT_PR", Value: strconv.FormatBool(task.Spec.DraftPR)},
		{Name: "DIVERGE_BUDGET_USD", Value: task.Spec.BudgetUSD},
		{Name: "DIVERGE_MAX_ITERATIONS", Value: strconv.Itoa(int(task.Spec.MaxIterations))},
		{Name: "DIVERGE_TOKEN_FILE", Value: "/etc/diverge/token/token"},
		{Name: "DIVERGE_MCP_URL", Value: serverBaseURL + "/mcp"},
		{Name: "DIVERGE_METER_URL", Value: serverBaseURL + "/api/v1/agenttask/meter"},
	}
	if guidance := strings.TrimSpace(task.Annotations["divergedev.com/guidance"]); guidance != "" {
		containerEnv = append(containerEnv, corev1.EnvVar{Name: "DIVERGE_GUIDANCE", Value: guidance})
	}

	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: task.Namespace,
			Labels: map[string]string{
				"divergedev.com/agent-task":       pkgauth.TaskLabelValue(task.Name),
				"app.kubernetes.io/managed-by":    "diverge",
				"divergedev.com/sandbox-provider": "pod",
			},
		},
		Spec: corev1.PodSpec{
			AutomountServiceAccountToken: ptr(false),
			RestartPolicy:                corev1.RestartPolicyNever,
			ActiveDeadlineSeconds:        ptr(timeoutSec),
			RuntimeClassName:             runtimeClassName,
			SecurityContext: &corev1.PodSecurityContext{
				RunAsNonRoot: ptr(true),
				RunAsUser:    ptr(int64(10001)),
				SeccompProfile: &corev1.SeccompProfile{
					Type: corev1.SeccompProfileTypeRuntimeDefault,
				},
			},
			Containers: []corev1.Container{
				{
					Name:  "agent",
					Image: image,
					Env:   containerEnv,
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("500m"),
							corev1.ResourceMemory: resource.MustParse("512Mi"),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse("2"),
							corev1.ResourceMemory: resource.MustParse("2Gi"),
						},
					},
					SecurityContext: &corev1.SecurityContext{
						AllowPrivilegeEscalation: ptr(false),
						ReadOnlyRootFilesystem:   ptr(true),
						Capabilities: &corev1.Capabilities{
							Drop: []corev1.Capability{"ALL"},
						},
					},
					VolumeMounts: []corev1.VolumeMount{
						{
							Name:      "workspace",
							MountPath: "/workspace",
						},
						{
							Name:      "tmp",
							MountPath: "/tmp",
						},
						{
							Name:      "token",
							MountPath: "/etc/diverge/token",
							ReadOnly:  true,
						},
					},
				},
			},
			Volumes: []corev1.Volume{
				{
					Name: "workspace",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: ptr(resource.MustParse("10Gi")),
						},
					},
				},
				{
					Name: "tmp",
					VolumeSource: corev1.VolumeSource{
						EmptyDir: &corev1.EmptyDirVolumeSource{
							SizeLimit: ptr(resource.MustParse("1Gi")),
						},
					},
				},
				{
					Name: "token",
					VolumeSource: corev1.VolumeSource{
						Secret: &corev1.SecretVolumeSource{
							SecretName:  pkgauth.SecretName(task.Name),
							DefaultMode: ptr(int32(0400)),
							Optional:    ptr(true),
						},
					},
				},
			},
		},
	}

	if p.scheme != nil {
		if err := controllerutil.SetControllerReference(task, pod, p.scheme); err != nil {
			return nil, fmt.Errorf("set controller reference: %w", err)
		}
	}

	if err := p.client.Create(ctx, pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			return nil, fmt.Errorf("create sandbox pod: %w", err)
		}
		existing := &corev1.Pod{}
		if getErr := p.client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: podName}, existing); getErr == nil {
			if existing.DeletionTimestamp != nil || existing.Status.Phase == corev1.PodFailed || existing.Status.Phase == corev1.PodSucceeded {
				return nil, fmt.Errorf("sandbox pod %s is terminating or in terminal phase %s; waiting for deletion", podName, existing.Status.Phase)
			}
		}
	}

	status, err := p.Status(ctx, task)
	if err != nil {
		return &pkgsandbox.SandboxResult{
			ClaimName:      podName,
			PodName:        podName,
			TokenMountPath: "/etc/diverge/token/token",
			Ready:          false,
			Message:        "Sandbox pod created; waiting for scheduling",
		}, nil
	}

	return &pkgsandbox.SandboxResult{
		ClaimName:      podName,
		PodName:        podName,
		PodIP:          status.PodIP,
		TokenMountPath: "/etc/diverge/token/token",
		Ready:          status.Ready,
		Message:        status.Message,
	}, nil
}

// Status inspects the sandbox Pod and resolves its phase and readiness.
func (p *PodSandboxProvider) Status(ctx context.Context, task *v1alpha1.AgentTask) (*pkgsandbox.SandboxStatus, error) {
	podName := SandboxPodName(task.Name)
	pod := &corev1.Pod{}
	if err := p.client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: podName}, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return &pkgsandbox.SandboxStatus{
				ClaimName: podName,
				Phase:     "Pending",
				Ready:     false,
				Message:   "Sandbox pod not found",
			}, nil
		}
		return nil, fmt.Errorf("get sandbox pod %s: %w", podName, err)
	}

	var phase string
	switch pod.Status.Phase {
	case corev1.PodRunning:
		phase = "Running"
	case corev1.PodSucceeded:
		phase = "Succeeded"
	case corev1.PodFailed:
		phase = "Failed"
	default:
		phase = "Pending"
	}

	isReady := false
	if pod.Status.Phase == corev1.PodRunning {
		for _, c := range pod.Status.Conditions {
			if c.Type == corev1.PodReady && c.Status == corev1.ConditionTrue {
				isReady = true
				break
			}
		}
	}

	return &pkgsandbox.SandboxStatus{
		ClaimName: podName,
		PodName:   pod.Name,
		PodIP:     pod.Status.PodIP,
		Phase:     phase,
		Ready:     isReady,
		Message:   fmt.Sprintf("Sandbox pod phase: %s", phase),
	}, nil
}

// Teardown deletes the sandbox Pod.
func (p *PodSandboxProvider) Teardown(ctx context.Context, task *v1alpha1.AgentTask) error {
	podName := SandboxPodName(task.Name)
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      podName,
			Namespace: task.Namespace,
		},
	}
	if err := p.client.Delete(ctx, pod); err != nil {
		if apierrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("delete sandbox pod %s: %w", podName, err)
	}
	return nil
}

// Suspend deletes the standalone sandbox Pod when paused to reclaim cluster compute.
func (p *PodSandboxProvider) Suspend(ctx context.Context, task *v1alpha1.AgentTask) error {
	return p.Teardown(ctx, task)
}

// Resume ensures a healthy sandbox Pod is running, re-provisioning if missing or in a terminal phase.
func (p *PodSandboxProvider) Resume(ctx context.Context, task *v1alpha1.AgentTask) error {
	podName := SandboxPodName(task.Name)
	pod := &corev1.Pod{}
	err := p.client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: podName}, pod)
	if err != nil {
		if apierrors.IsNotFound(err) {
			_, provErr := p.Provision(ctx, task)
			return provErr
		}
		return fmt.Errorf("get sandbox pod %s on resume: %w", podName, err)
	}
	if pod.DeletionTimestamp != nil {
		return fmt.Errorf("sandbox pod %s is terminating; waiting for deletion before reprovisioning", podName)
	}
	if pod.Status.Phase == corev1.PodFailed || pod.Status.Phase == corev1.PodSucceeded {
		if delErr := p.Teardown(ctx, task); delErr != nil {
			return delErr
		}
		if getErr := p.client.Get(ctx, client.ObjectKey{Namespace: task.Namespace, Name: podName}, &corev1.Pod{}); getErr == nil || !apierrors.IsNotFound(getErr) {
			return fmt.Errorf("sandbox pod %s deletion in progress; retry reprovisioning once deleted", podName)
		}
		_, provErr := p.Provision(ctx, task)
		return provErr
	}
	return nil
}

// StreamLogs streams the logs of the sandbox Pod.
func (p *PodSandboxProvider) StreamLogs(ctx context.Context, task *v1alpha1.AgentTask, opts pkgsandbox.LogOptions) (io.ReadCloser, error) {
	podName := SandboxPodName(task.Name)

	if p.clientset == nil {
		return io.NopCloser(strings.NewReader("clientset not available for log streaming\n")), nil
	}

	podLogOpts := &corev1.PodLogOptions{
		Follow:     opts.Follow,
		Timestamps: opts.Timestamps,
	}
	if opts.TailLines != nil {
		podLogOpts.TailLines = opts.TailLines
	}

	streamCtx, cancel := context.WithCancel(ctx)
	req := p.clientset.CoreV1().Pods(task.Namespace).GetLogs(podName, podLogOpts)
	stream, err := req.Stream(streamCtx)
	if err != nil {
		cancel()
		return nil, err
	}
	return &cancelableReadCloser{ReadCloser: stream, cancel: cancel}, nil
}

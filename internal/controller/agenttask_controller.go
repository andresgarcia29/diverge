package controller

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/auth"
	"github.com/divergedev/diverge/pkg/registry"
	pkgsandbox "github.com/divergedev/diverge/pkg/sandbox"
)

const (
	agentTaskFinalizer    = "divergedev.com/agenttask-finalizer"
	annotationActiveSince = "divergedev.com/active-since"
)

func activeStartTime(task *v1alpha1.AgentTask) time.Time {
	if task == nil {
		return time.Time{}
	}
	if task.Annotations != nil {
		if raw := task.Annotations[annotationActiveSince]; raw != "" {
			if parsed, err := time.Parse(time.RFC3339, raw); err == nil {
				return parsed
			}
		}
	}
	return task.CreationTimestamp.Time
}

// TaskNotifier notifies external observers when an AgentTask changes phases.
type TaskNotifier interface {
	NotifyPhaseChange(ctx context.Context, task *v1alpha1.AgentTask, oldPhase, newPhase v1alpha1.AgentTaskPhase) error
}

// NoopTaskNotifier is a default no-op implementation of TaskNotifier.
type NoopTaskNotifier struct{}

// NotifyPhaseChange does nothing.
func (n *NoopTaskNotifier) NotifyPhaseChange(_ context.Context, _ *v1alpha1.AgentTask, _, _ v1alpha1.AgentTaskPhase) error {
	return nil
}

// SandboxLifecycleHook allows executing custom logic before/after provisioning and before teardown.
type SandboxLifecycleHook interface {
	PreProvision(ctx context.Context, task *v1alpha1.AgentTask) error
	PostProvision(ctx context.Context, task *v1alpha1.AgentTask, res *pkgsandbox.SandboxResult) error
	PreTeardown(ctx context.Context, task *v1alpha1.AgentTask) error
}

// FeatureGate provides dynamic feature enablement queries.
type FeatureGate interface {
	IsEnabled(ctx context.Context, feature string) bool
}

// StaticFeatureGate is a static implementation of FeatureGate.
type StaticFeatureGate struct {
	Enabled bool
}

// IsEnabled returns the configured static enablement state.
func (g *StaticFeatureGate) IsEnabled(_ context.Context, _ string) bool {
	return g.Enabled
}

// AgentTaskReconciler reconciles an AgentTask object.
type AgentTaskReconciler struct {
	client.Client
	Clientset       kubernetes.Interface
	Scheme          *runtime.Scheme
	SandboxRegistry *registry.Registry[pkgsandbox.SandboxProvider]
	TokenMinter     auth.Minter
	TokenStore      auth.TokenStore
	TokenAuditor    auth.TokenAuditor
	Notifier        TaskNotifier
	LifecycleHook   SandboxLifecycleHook
	FeatureGate     FeatureGate
	DefaultProvider string
}

// +kubebuilder:rbac:groups=divergedev.com,resources=agenttasks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=divergedev.com,resources=agenttasks/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=divergedev.com,resources=agenttasks/finalizers,verbs=update
// +kubebuilder:rbac:groups=extensions.agents.x-k8s.io,resources=sandboxclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=agents.x-k8s.io,resources=sandboxes,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;create;update;patch;delete

// Reconcile manages the AgentTask lifecycle.
func (r *AgentTaskReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	logger := log.FromContext(ctx).WithValues("agenttask", req.NamespacedName)

	task := &v1alpha1.AgentTask{}
	if err := r.Get(ctx, req.NamespacedName, task); err != nil {
		if apierrors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	// 1. Handle Deletion
	if !task.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(task, agentTaskFinalizer) {
			logger.Info("Tearing down AgentTask sandbox", "task", task.Name)
			if r.LifecycleHook != nil {
				if err := r.LifecycleHook.PreTeardown(ctx, task); err != nil {
					logger.Error(err, "PreTeardown lifecycle hook failed")
				}
			}

			provider, err := r.resolveSandboxProvider(task)
			if err == nil && provider != nil {
				if err := provider.Teardown(ctx, task); err != nil {
					logger.Error(err, "Failed to teardown sandbox")
					return ctrl.Result{RequeueAfter: 5 * time.Second}, err
				}
			}

			store := r.resolveTokenStore()
			delErr := store.DeleteToken(ctx, task.Name, task.Namespace)
			if delErr != nil && !errors.Is(delErr, auth.ErrTokenNotFound) {
				logger.Error(delErr, "Failed to delete token")
				if r.TokenAuditor != nil {
					_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
						Type:      auth.EventRevoke,
						TaskID:    task.Name,
						Namespace: task.Namespace,
						Principal: "diverge-controller",
						Success:   false,
						Reason:    delErr.Error(),
						Timestamp: time.Now(),
					})
				}
				return ctrl.Result{RequeueAfter: 5 * time.Second}, delErr
			}

			if r.TokenAuditor != nil {
				_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
					Type:      auth.EventRevoke,
					TaskID:    task.Name,
					Namespace: task.Namespace,
					Principal: "diverge-controller",
					Success:   true,
					Timestamp: time.Now(),
				})
			}

			controllerutil.RemoveFinalizer(task, agentTaskFinalizer)
			if err := r.Update(ctx, task); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 2. Ensure Finalizer
	if !controllerutil.ContainsFinalizer(task, agentTaskFinalizer) {
		controllerutil.AddFinalizer(task, agentTaskFinalizer)
		if err := r.Update(ctx, task); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 3. Dynamic Feature Gate Check
	if r.FeatureGate != nil && !r.FeatureGate.IsEnabled(ctx, "agent_sandbox") {
		if task.Status.Phase != v1alpha1.AgentTaskPhasePaused {
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			task.Status.Phase = v1alpha1.AgentTaskPhasePaused
			task.Status.Message = "Agent sandbox execution paused: feature gate 'agent_sandbox' is disabled"
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
		}
		return ctrl.Result{}, nil
	}

	// 4. Handle Suspended State
	if task.Spec.Suspended {
		if provider, err := r.resolveSandboxProvider(task); err == nil && provider != nil {
			if suspender, ok := provider.(pkgsandbox.SandboxSuspender); ok {
				if err := suspender.Suspend(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
			} else if task.Spec.Sandbox.Provider == "pod" || task.Spec.Sandbox.Provider == "k8s-pod" {
				if err := provider.Teardown(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
			}
		}
		if err := r.setLinkedPreviewEnvironmentHibernated(ctx, task, true); err != nil {
			return ctrl.Result{}, err
		}
		if task.Status.Phase != v1alpha1.AgentTaskPhasePaused {
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			oldPhase := task.Status.Phase
			task.Status.Phase = v1alpha1.AgentTaskPhasePaused
			if task.Status.Message == "" || task.Status.Message == "Sandbox active and ready" || task.Status.Message == "Task resumed and active" {
				task.Status.Message = "Task execution paused by spec.suspended"
			}
			if task.Spec.Sandbox.Provider == "pod" || task.Spec.Sandbox.Provider == "k8s-pod" {
				task.Status.SandboxPodName = ""
			}
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
			if r.Notifier != nil && oldPhase != task.Status.Phase {
				_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
			}
		}
		if err := r.revokeToken(ctx, task, "paused by spec.suspended"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	// 5. Resolve Sandbox Provider
	provider, err := r.resolveSandboxProvider(task)
	if err != nil {
		patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
		oldPhase := task.Status.Phase
		task.Status.Phase = v1alpha1.AgentTaskPhaseFailed
		task.Status.Message = fmt.Sprintf("Failed to resolve sandbox provider: %v", err)
		_ = r.Status().Patch(ctx, task, patch)
		if r.Notifier != nil && oldPhase != task.Status.Phase {
			_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
		}
		return ctrl.Result{}, err
	}

	// 6. Lifecycle State Machine
	switch task.Status.Phase {
	case "", v1alpha1.AgentTaskPhasePending:
		logger.Info("Provisioning sandbox for AgentTask", "task", task.Name)

		if err := r.mintAndSaveToken(ctx, task); err != nil {
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			task.Status.Phase = v1alpha1.AgentTaskPhasePending
			task.Status.Message = fmt.Sprintf("Token provisioning failed (retrying): %v", err)
			_ = r.Status().Patch(ctx, task, patch)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}

		if r.LifecycleHook != nil {
			if err := r.LifecycleHook.PreProvision(ctx, task); err != nil {
				logger.Error(err, "PreProvision lifecycle hook failed")
			}
		}

		res, err := provider.Provision(ctx, task)
		if err != nil {
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			task.Status.Phase = v1alpha1.AgentTaskPhasePending
			task.Status.Message = fmt.Sprintf("Provisioning failed (retrying): %v", err)
			_ = r.Status().Patch(ctx, task, patch)
			return ctrl.Result{RequeueAfter: 10 * time.Second}, err
		}

		if r.LifecycleHook != nil {
			if err := r.LifecycleHook.PostProvision(ctx, task, res); err != nil {
				logger.Error(err, "PostProvision lifecycle hook failed")
			}
		}

		baseTask := task.DeepCopy()
		oldPhase := task.Status.Phase
		task.Status.SandboxClaimRef = res.ClaimName
		task.Status.SandboxPodName = res.PodName
		task.Status.SandboxIP = res.PodIP
		task.Status.Message = res.Message
		if err := r.reconcileLinkedPreviewEnvironment(ctx, task); err != nil {
			logger.Error(err, "Failed to reconcile linked preview environment")
		}
		baseTask.ObjectMeta = *task.ObjectMeta.DeepCopy()

		patch := client.MergeFromWithOptions(baseTask, client.MergeFromWithOptimisticLock{})
		if res.Ready {
			task.Status.Phase = v1alpha1.AgentTaskPhaseActive
		} else {
			task.Status.Phase = v1alpha1.AgentTaskPhaseProvisioning
		}

		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
		}
		if r.Notifier != nil && oldPhase != task.Status.Phase {
			_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
		}
		return ctrl.Result{RequeueAfter: 3 * time.Second}, nil

	case v1alpha1.AgentTaskPhaseProvisioning:
		sbStatus, err := provider.Status(ctx, task)
		if err != nil {
			logger.Error(err, "Error polling sandbox status")
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}

		if handled, res, termErr := r.handleTerminalSandboxPhase(ctx, task, sbStatus); handled {
			return res, termErr
		}

		baseTask := task.DeepCopy()
		oldPhase := task.Status.Phase
		task.Status.SandboxPodName = sbStatus.PodName
		task.Status.SandboxIP = sbStatus.PodIP
		task.Status.Message = sbStatus.Message
		if err := r.reconcileLinkedPreviewEnvironment(ctx, task); err != nil {
			logger.Error(err, "Failed to reconcile linked preview environment")
		}
		baseTask.ObjectMeta = *task.ObjectMeta.DeepCopy()
		patch := client.MergeFromWithOptions(baseTask, client.MergeFromWithOptimisticLock{})
		if sbStatus.Ready {
			task.Status.Phase = v1alpha1.AgentTaskPhaseActive
			task.Status.Message = "Sandbox active and ready"
		}

		if err := r.Status().Patch(ctx, task, patch); err != nil {
			return ctrl.Result{}, err
		}
		if r.Notifier != nil && oldPhase != task.Status.Phase {
			_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
		}
		if !sbStatus.Ready {
			return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
		}
		return ctrl.Result{RequeueAfter: 10 * time.Second}, nil

	case v1alpha1.AgentTaskPhaseActive:
		// Check sandbox execution timeout from active start time (or CreationTimestamp)
		startTime := activeStartTime(task)
		if task.Spec.Sandbox.TimeoutSeconds > 0 && !startTime.IsZero() {
			if time.Since(startTime) > time.Duration(task.Spec.Sandbox.TimeoutSeconds)*time.Second {
				if err := provider.Teardown(ctx, task); err != nil {
					logger.Error(err, "Failed to teardown timed-out sandbox")
					_ = r.revokeToken(ctx, task, "sandbox timeout exceeded")
					return ctrl.Result{RequeueAfter: 5 * time.Second}, err
				}
				patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
				oldPhase := task.Status.Phase
				task.Status.Phase = v1alpha1.AgentTaskPhaseFailed
				task.Status.Message = fmt.Sprintf("Sandbox execution timed out after %ds", task.Spec.Sandbox.TimeoutSeconds)
				if err := r.Status().Patch(ctx, task, patch); err != nil {
					return ctrl.Result{}, err
				}
				if r.Notifier != nil && oldPhase != task.Status.Phase {
					_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
				}
				_ = r.setLinkedPreviewEnvironmentHibernated(ctx, task, true)
				if err := r.revokeToken(ctx, task, "sandbox timeout exceeded"); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
		}

		// Check iteration, USD budget, and token budget limits
		if exceeded, reason := r.checkBudgetOrIterationLimit(task); exceeded {
			if suspender, ok := provider.(pkgsandbox.SandboxSuspender); ok {
				if err := suspender.Suspend(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
			} else if task.Spec.Sandbox.Provider == "pod" || task.Spec.Sandbox.Provider == "k8s-pod" {
				if err := provider.Teardown(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
			}
			if err := r.setLinkedPreviewEnvironmentHibernated(ctx, task, true); err != nil {
				return ctrl.Result{}, err
			}
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			oldPhase := task.Status.Phase
			task.Status.Phase = v1alpha1.AgentTaskPhasePaused
			task.Status.Message = reason
			if task.Spec.Sandbox.Provider == "pod" || task.Spec.Sandbox.Provider == "k8s-pod" {
				task.Status.SandboxPodName = ""
			}
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
			if r.Notifier != nil && oldPhase != task.Status.Phase {
				_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
			}
			if err := r.revokeToken(ctx, task, reason); err != nil {
				return ctrl.Result{}, err
			}
			return ctrl.Result{}, nil
		}

		// Re-mint token if budgetUSD or budgetTokens was bumped while Active (e.g. via /diverge budget)
		if err := r.ensureActiveTokenMatchesSpec(ctx, task); err != nil {
			logger.Error(err, "Failed to refresh token after spec budget update")
		}

		// Periodic health check, terminal sandbox detection, and linked preview environment sync
		sbStatus, err := provider.Status(ctx, task)
		if err == nil && sbStatus != nil {
			if handled, res, termErr := r.handleTerminalSandboxPhase(ctx, task, sbStatus); handled {
				return res, termErr
			}
		}
		baseTask := task.DeepCopy()
		if err == nil && sbStatus != nil {
			if sbStatus.PodName != "" {
				task.Status.SandboxPodName = sbStatus.PodName
			}
			if sbStatus.PodIP != "" {
				task.Status.SandboxIP = sbStatus.PodIP
			}
		}
		if err := r.reconcileLinkedPreviewEnvironment(ctx, task); err != nil {
			logger.Error(err, "Failed to reconcile linked preview environment")
		}
		baseTask.ObjectMeta = *task.ObjectMeta.DeepCopy()
		patch := client.MergeFromWithOptions(baseTask, client.MergeFromWithOptimisticLock{})

		_ = r.Status().Patch(ctx, task, patch)
		return ctrl.Result{RequeueAfter: 15 * time.Second}, nil

	case v1alpha1.AgentTaskPhasePaused:
		if !task.Spec.Suspended {
			if exceeded, reason := r.checkBudgetOrIterationLimit(task); exceeded {
				if err := r.revokeToken(ctx, task, reason); err != nil {
					return ctrl.Result{}, err
				}
				return ctrl.Result{}, nil
			}
			if task.Spec.Sandbox.TimeoutSeconds > 0 && !task.CreationTimestamp.IsZero() &&
				time.Since(task.CreationTimestamp.Time) > time.Duration(task.Spec.Sandbox.TimeoutSeconds)*time.Second {
				metaPatch := client.MergeFrom(task.DeepCopy())
				if task.Annotations == nil {
					task.Annotations = make(map[string]string)
				}
				task.Annotations[annotationActiveSince] = time.Now().UTC().Format(time.RFC3339)
				savedStatus := task.Status.DeepCopy()
				if err := r.Patch(ctx, task, metaPatch); err != nil {
					return ctrl.Result{}, err
				}
				restoreReconcilerStatus(task, savedStatus)
			}
			if err := r.setLinkedPreviewEnvironmentHibernated(ctx, task, false); err != nil {
				return ctrl.Result{}, err
			}
			patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
			oldPhase := task.Status.Phase
			if task.Status.SandboxPodName != "" {
				if sbStatus, sbErr := provider.Status(ctx, task); sbErr == nil && sbStatus != nil {
					if sbStatus.Phase == "Failed" || sbStatus.Phase == "Succeeded" || sbStatus.Message == "Sandbox pod not found" || sbStatus.Message == "SandboxClaim not found" {
						task.Status.SandboxPodName = ""
					}
				}
			}
			if task.Status.SandboxPodName != "" {
				// Re-mint capability token with updated budget/token limits and fresh expiry
				if err := r.mintAndSaveToken(ctx, task); err != nil {
					return ctrl.Result{}, err
				}
				if suspender, ok := provider.(pkgsandbox.SandboxSuspender); ok {
					if err := suspender.Resume(ctx, task); err != nil {
						return ctrl.Result{}, err
					}
				}
				task.Status.Phase = v1alpha1.AgentTaskPhaseActive
				task.Status.Message = "Task resumed and active"
			} else {
				task.Status.Phase = v1alpha1.AgentTaskPhasePending
				task.Status.Message = "Task resumed, awaiting provisioning"
			}
			task.Status.LastMeterRequestIDs = nil
			if err := r.Status().Patch(ctx, task, patch); err != nil {
				return ctrl.Result{}, err
			}
			if r.Notifier != nil && oldPhase != task.Status.Phase {
				_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
			}
			return ctrl.Result{RequeueAfter: 1 * time.Second}, nil
		}
		return ctrl.Result{}, nil

	case v1alpha1.AgentTaskPhaseCompleted, v1alpha1.AgentTaskPhaseFailed:
		if err := r.revokeToken(ctx, task, "task in terminal phase"); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	return ctrl.Result{}, nil
}

func (r *AgentTaskReconciler) handleTerminalSandboxPhase(ctx context.Context, task *v1alpha1.AgentTask, sbStatus *pkgsandbox.SandboxStatus) (bool, ctrl.Result, error) {
	if sbStatus == nil || (sbStatus.Phase != "Succeeded" && sbStatus.Phase != "Failed") {
		return false, ctrl.Result{}, nil
	}
	patch := client.MergeFromWithOptions(task.DeepCopy(), client.MergeFromWithOptimisticLock{})
	oldPhase := task.Status.Phase
	if sbStatus.PodName != "" {
		task.Status.SandboxPodName = sbStatus.PodName
	}
	if sbStatus.PodIP != "" {
		task.Status.SandboxIP = sbStatus.PodIP
	}
	var revokeReason string
	if sbStatus.Phase == "Succeeded" {
		task.Status.Phase = v1alpha1.AgentTaskPhaseCompleted
		task.Status.Message = "Agent task completed successfully"
		revokeReason = "task completed"
	} else {
		task.Status.Phase = v1alpha1.AgentTaskPhaseFailed
		if sbStatus.Message != "" {
			task.Status.Message = sbStatus.Message
		} else {
			task.Status.Message = "Sandbox execution failed"
		}
		revokeReason = "task failed"
	}
	if err := r.Status().Patch(ctx, task, patch); err != nil {
		return true, ctrl.Result{}, err
	}
	if r.Notifier != nil && oldPhase != task.Status.Phase {
		_ = r.Notifier.NotifyPhaseChange(ctx, task, oldPhase, task.Status.Phase)
	}
	_ = r.setLinkedPreviewEnvironmentHibernated(ctx, task, true)
	if err := r.revokeToken(ctx, task, revokeReason); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{}, nil
}

func (r *AgentTaskReconciler) ensureActiveTokenMatchesSpec(ctx context.Context, task *v1alpha1.AgentTask) error {
	verifier, ok := r.TokenMinter.(*auth.MacaroonProvider)
	if !ok || verifier == nil {
		return nil
	}
	store := r.resolveTokenStore()
	rawTok, err := store.GetToken(ctx, task.Name, task.Namespace)
	if err != nil {
		return r.mintAndSaveToken(ctx, task)
	}
	_, claims, err := verifier.VerifyAndExtract(ctx, rawTok)
	if err != nil {
		return r.mintAndSaveToken(ctx, task)
	}
	var specCost float64
	if task.Spec.BudgetUSD != "" {
		if parsed, parseErr := strconv.ParseFloat(task.Spec.BudgetUSD, 64); parseErr == nil {
			specCost = parsed
		}
	}
	if claims.MaxTokens != task.Spec.BudgetTokens || math.Abs(claims.MaxCostUSD-specCost) > 1e-6 {
		return r.mintAndSaveToken(ctx, task)
	}
	return nil
}

// checkBudgetOrIterationLimit reports whether the AgentTask has reached its configured
// maxIterations, budgetUSD, or budgetTokens limit and returns the human-readable pause reason.
func (r *AgentTaskReconciler) checkBudgetOrIterationLimit(task *v1alpha1.AgentTask) (bool, string) {
	if task.Spec.MaxIterations > 0 && task.Status.Iteration > task.Spec.MaxIterations {
		return true, fmt.Sprintf("Reached max iterations limit (%d); paused", task.Spec.MaxIterations)
	}
	if task.Spec.BudgetUSD != "" && task.Status.CostUSD != "" {
		budget, err1 := strconv.ParseFloat(task.Spec.BudgetUSD, 64)
		cost, err2 := strconv.ParseFloat(task.Status.CostUSD, 64)
		if err1 == nil && err2 == nil && budget > 0 && cost >= budget {
			return true, fmt.Sprintf("Reached USD budget limit ($%s / $%s); paused", task.Status.CostUSD, task.Spec.BudgetUSD)
		}
	}
	if task.Spec.BudgetTokens > 0 && task.Status.TokensConsumed >= task.Spec.BudgetTokens {
		return true, fmt.Sprintf("Reached token budget limit (%d / %d); paused", task.Status.TokensConsumed, task.Spec.BudgetTokens)
	}
	return false, ""
}

// mintAndSaveToken mints a namespace- and task-bound Macaroon capability token for the
// given AgentTask and persists it via the configured TokenStore.
func (r *AgentTaskReconciler) mintAndSaveToken(ctx context.Context, task *v1alpha1.AgentTask) error {
	if r.TokenMinter == nil {
		return nil
	}
	logger := log.FromContext(ctx).WithValues("agenttask", client.ObjectKeyFromObject(task))
	timeoutSec := task.Spec.Sandbox.TimeoutSeconds
	if timeoutSec <= 0 {
		timeoutSec = 3600
	}
	claims := auth.Claims{
		TaskID:    task.Name,
		Namespace: task.Namespace,
		RepoURL:   task.Spec.Repository.URL,
		Branch:    task.Spec.Repository.BaseBranch,
		AllowedTools: []string{
			"diverge_*_environment*",
			"diverge_*_preview_group*",
			"diverge_wait_for_ready",
			"diverge_fetch_errors",
			"diverge_list_hook_jobs",
			"diverge_retry_hook",
			"diverge_extend_ttl",
			"diverge_doctor",
			"diverge_verify",
			"diverge_loadtest",
			"diverge_visual_test",
			"diverge_replay",
			"diverge_get_task",
			"diverge_list_tasks",
		},
		AllowedModels: task.Spec.Capabilities,
		MaxTokens:     task.Spec.BudgetTokens,
		ExpiresAt:     time.Now().Add(time.Duration(timeoutSec) * time.Second),
	}
	if task.Spec.BudgetUSD != "" {
		if cost, err := strconv.ParseFloat(task.Spec.BudgetUSD, 64); err == nil {
			claims.MaxCostUSD = cost
		}
	}

	tok, err := r.TokenMinter.Mint(ctx, claims)
	if err != nil {
		logger.Error(err, "Failed to mint task token")
		if r.TokenAuditor != nil {
			_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
				Type:      auth.EventMint,
				TaskID:    task.Name,
				Namespace: task.Namespace,
				Principal: "diverge-controller",
				Success:   false,
				Reason:    err.Error(),
				Timestamp: time.Now(),
			})
		}
		return fmt.Errorf("mint task token: %w", err)
	}

	tokBytes, err := tok.Serialize()
	if err != nil {
		logger.Error(err, "Failed to serialize task token")
		if r.TokenAuditor != nil {
			_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
				Type:      auth.EventMint,
				TaskID:    task.Name,
				Namespace: task.Namespace,
				Principal: "diverge-controller",
				Success:   false,
				Reason:    err.Error(),
				Timestamp: time.Now(),
			})
		}
		return fmt.Errorf("serialize task token: %w", err)
	}

	store := r.resolveTokenStore()
	saveErr := store.SaveToken(ctx, task.Name, task.Namespace, tokBytes)
	reason := ""
	if saveErr != nil {
		logger.Error(saveErr, "Failed to persist task token via TokenStore")
		reason = saveErr.Error()
	}
	if r.TokenAuditor != nil {
		_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
			Type:      auth.EventMint,
			TaskID:    task.Name,
			Namespace: task.Namespace,
			Principal: "diverge-controller",
			Success:   saveErr == nil,
			Reason:    reason,
			Timestamp: time.Now(),
		})
	}
	if saveErr != nil {
		return fmt.Errorf("persist task token: %w", saveErr)
	}
	return nil
}

// revokeToken marks the AgentTask's capability token as revoked in the TokenStore and records an audit event.
// Missing or already-revoked tokens are treated as idempotent success.
func (r *AgentTaskReconciler) revokeToken(ctx context.Context, task *v1alpha1.AgentTask, reason string) error {
	store := r.resolveTokenStore()
	if store.IsRevoked(ctx, task.Name, task.Namespace) {
		return nil
	}
	if err := store.RevokeToken(ctx, task.Name, task.Namespace); err != nil {
		if errors.Is(err, auth.ErrTokenNotFound) || errors.Is(err, auth.ErrTokenRevoked) {
			return nil
		}
		return fmt.Errorf("revoke task token: %w", err)
	}
	if r.TokenAuditor != nil {
		_ = r.TokenAuditor.RecordEvent(ctx, auth.AuthEvent{
			Type:      auth.EventRevoke,
			TaskID:    task.Name,
			Namespace: task.Namespace,
			Principal: "diverge-controller",
			Success:   true,
			Reason:    reason,
			Timestamp: time.Now(),
		})
	}
	return nil
}

// resolveTokenStore returns the configured TokenStore or defaults to KubernetesSecretTokenStore.
func (r *AgentTaskReconciler) resolveTokenStore() auth.TokenStore {
	if r.TokenStore != nil {
		return r.TokenStore
	}
	return auth.NewKubernetesSecretTokenStore(r.Client, r.Scheme)
}

// resolveSandboxProvider resolves and instantiates the SandboxProvider for the given AgentTask.
func (r *AgentTaskReconciler) resolveSandboxProvider(task *v1alpha1.AgentTask) (pkgsandbox.SandboxProvider, error) {
	providerName := task.Spec.Sandbox.Provider
	if providerName == "" {
		if r.DefaultProvider != "" {
			providerName = r.DefaultProvider
		} else {
			providerName = "agent-sandbox"
		}
	}

	reg := r.SandboxRegistry
	if reg == nil {
		reg = pkgsandbox.Providers
	}

	if !reg.Has(providerName) {
		return nil, fmt.Errorf("sandbox provider %q not registered", providerName)
	}

	return reg.Create(providerName, registry.Deps{
		Client:    r.Client,
		Clientset: r.Clientset,
		Scheme:    r.Scheme,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *AgentTaskReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.Clientset == nil && mgr.GetConfig() != nil {
		if cs, err := kubernetes.NewForConfig(mgr.GetConfig()); err == nil {
			r.Clientset = cs
		}
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.AgentTask{}).
		Complete(r)
}

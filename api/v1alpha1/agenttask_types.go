package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// AgentTaskPhase represents the lifecycle phase of an AgentTask.
type AgentTaskPhase string

const (
	// AgentTaskPhasePending indicates the task was submitted and is awaiting provisioning.
	AgentTaskPhasePending AgentTaskPhase = "Pending"
	// AgentTaskPhaseProvisioning indicates the sandbox pod and environment are being provisioned.
	AgentTaskPhaseProvisioning AgentTaskPhase = "Provisioning"
	// AgentTaskPhaseActive indicates the agent is running in the sandbox and iterating.
	AgentTaskPhaseActive AgentTaskPhase = "Active"
	// AgentTaskPhasePausing indicates a pause has been requested and checkpointing is in progress.
	AgentTaskPhasePausing AgentTaskPhase = "Pausing"
	// AgentTaskPhasePaused indicates the agent is suspended, compute is scaled down.
	AgentTaskPhasePaused AgentTaskPhase = "Paused"
	// AgentTaskPhaseCompleted indicates the task successfully completed and opened a draft PR.
	AgentTaskPhaseCompleted AgentTaskPhase = "Completed"
	// AgentTaskPhaseFailed indicates the task encountered an unrecoverable failure.
	AgentTaskPhaseFailed AgentTaskPhase = "Failed"
)

// AgentTaskRepository defines the target repository and branch parameters.
type AgentTaskRepository struct {
	// URL is the Git clone URL (e.g. https://github.com/org/repo or git@github.com:org/repo.git).
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	URL string `json:"url"`

	// BaseBranch is the baseline branch to fork from and submit PR against (defaults to "main").
	// +kubebuilder:default="main"
	// +optional
	BaseBranch string `json:"baseBranch,omitempty"`

	// WorkingBranch is the branch where the agent commits its work.
	// Defaults to "agent/<task-name>".
	// +optional
	WorkingBranch string `json:"workingBranch,omitempty"`
}

// AgentTaskSandbox configures the isolated sandbox execution environment.
type AgentTaskSandbox struct {
	// Provider defines the sandbox provider kind. Defaults to "agent-sandbox".
	// +kubebuilder:default="agent-sandbox"
	// +optional
	Provider string `json:"provider,omitempty"`

	// PoolRef references a pre-warmed SandboxWarmPool in the cluster.
	// +optional
	PoolRef string `json:"poolRef,omitempty"`

	// TemplateRef references a SandboxTemplate definition.
	// +optional
	TemplateRef string `json:"templateRef,omitempty"`

	// TimeoutSeconds is the execution TTL for the sandbox. Defaults to 3600 (1 hour).
	// +kubebuilder:default=3600
	// +optional
	TimeoutSeconds int32 `json:"timeoutSeconds,omitempty"`
}

// AgentTaskSpec defines the desired execution state of an AgentTask.
type AgentTaskSpec struct {
	// Objective is the human-provided natural language task or specification.
	// +kubebuilder:validation:Required
	// +kubebuilder:validation:MinLength=1
	Objective string `json:"objective"`

	// Repository specifies the target VCS repository.
	// +kubebuilder:validation:Required
	Repository AgentTaskRepository `json:"repository"`

	// Sandbox configures the isolated sandbox execution runtime.
	// +optional
	Sandbox AgentTaskSandbox `json:"sandbox,omitempty"`

	// BudgetUSD specifies the maximum token and compute spend limit (e.g. "5.00").
	// +optional
	BudgetUSD string `json:"budgetUSD,omitempty"`

	// BudgetTokens specifies the maximum token consumption limit (e.g. 500000).
	// +optional
	BudgetTokens int64 `json:"budgetTokens,omitempty"`

	// MaxIterations sets the maximum number of autonomous build-test-review loops.
	// Defaults to 5.
	// +kubebuilder:default=5
	// +kubebuilder:validation:Minimum=1
	// +optional
	MaxIterations int32 `json:"maxIterations,omitempty"`

	// Capabilities defines allowed model tiers (e.g. ["fast", "lite", "smart"]).
	// +optional
	Capabilities []string `json:"capabilities,omitempty"`

	// DraftPR specifies whether PRs created by the agent must be in Draft mode.
	// Defaults to true for safety.
	// +kubebuilder:default=true
	// +optional
	DraftPR bool `json:"draftPR,omitempty"`

	// Suspended pauses the agent execution and scales compute to zero.
	// +optional
	Suspended bool `json:"suspended,omitempty"`
}

// AgentTaskStatus defines the observed state of an AgentTask.
type AgentTaskStatus struct {
	// Phase is the current lifecycle state.
	// +kubebuilder:default="Pending"
	Phase AgentTaskPhase `json:"phase,omitempty"`

	// SandboxClaimRef is the name of the SandboxClaim created for this task.
	// +optional
	SandboxClaimRef string `json:"sandboxClaimRef,omitempty"`

	// SandboxPodName is the pod executing the sandbox workspace.
	// +optional
	SandboxPodName string `json:"sandboxPodName,omitempty"`

	// SandboxIP is the IP address of the sandbox pod.
	// +optional
	SandboxIP string `json:"sandboxIP,omitempty"`

	// Iteration tracks the current autonomous loop cycle.
	// +optional
	Iteration int32 `json:"iteration,omitempty"`

	// PRURL is the URL of the created draft pull request once opened.
	// +optional
	PRURL string `json:"prURL,omitempty"`

	// CostUSD is the accumulated token/compute cost in USD.
	// +optional
	CostUSD string `json:"costUSD,omitempty"`

	// TokensConsumed is the accumulated count of prompt and candidate tokens consumed.
	// +optional
	TokensConsumed int64 `json:"tokensConsumed,omitempty"`

	// Message is human-readable status or diagnostic text.
	// +optional
	Message string `json:"message,omitempty"`

	// Conditions describe state transitions and status observations.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`

	// LastMeterRequestIDs stores a bounded ring buffer (max 32 entries) of recently processed
	// LLM meter request IDs to guarantee idempotency across retries and server replicas.
	// +optional
	LastMeterRequestIDs []string `json:"lastMeterRequestIds,omitempty"`

	// ObservedGeneration is the most recent metadata generation observed by the controller.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type="string",JSONPath=".status.phase",description="Agent task lifecycle phase"
// +kubebuilder:printcolumn:name="Sandbox",type="string",JSONPath=".status.sandboxPodName",description="Sandbox pod name"
// +kubebuilder:printcolumn:name="PR",type="string",JSONPath=".status.prURL",description="Draft PR URL"
// +kubebuilder:printcolumn:name="Age",type="date",JSONPath=".metadata.creationTimestamp"

// AgentTask represents an autonomous AI development task in Diverge.
// It orchestrates an isolated sandbox, code execution, automated testing against preview
// environments, and draft PR generation.
type AgentTask struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   AgentTaskSpec   `json:"spec,omitempty"`
	Status AgentTaskStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// AgentTaskList contains a list of AgentTasks.
type AgentTaskList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []AgentTask `json:"items"`
}

func init() {
	SchemeBuilder.Register(&AgentTask{}, &AgentTaskList{})
}

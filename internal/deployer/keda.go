package deployer

import (
	"context"
	"fmt"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/log"

	"github.com/divergedev/diverge/api/v1alpha1"
)

var hsoGVK = schema.GroupVersionKind{
	Group:   "http.keda.sh",
	Version: "v1alpha1",
	Kind:    "HTTPScaledObject",
}

var hsoListGVK = schema.GroupVersionKind{
	Group:   "http.keda.sh",
	Version: "v1alpha1",
	Kind:    "HTTPScaledObjectList",
}

// KEDAConfig holds controller-level CLI defaults for KEDA autoscaling.
// Per-service CRD config (KEDASpec) overrides these when set.
type KEDAConfig struct {
	MinReplicas int64
	MaxReplicas int64
	Cooldown    int64
}

// KEDADeployer represents the configuration or state for this type.
type KEDADeployer struct {
	Inner  Deployer
	Client client.Client
	Config KEDAConfig
}

// Deploy performs its designated operation.
func (d *KEDADeployer) Deploy(ctx context.Context, env *v1alpha1.Environment) error {
	logger := log.FromContext(ctx).WithName("keda-deployer")

	// Validate KEDA configuration before deploying the workload to avoid
	// partial deployments with invalid autoscaling settings.
	minRepl, maxRepl, cooldown := d.resolveKEDAConfig(env)
	if minRepl > maxRepl {
		return fmt.Errorf("invalid KEDA config: minReplicas (%d) > maxReplicas (%d)", minRepl, maxRepl)
	}

	if err := d.Inner.Deploy(ctx, env); err != nil {
		return err
	}

	// Check if KEDA CRD exists
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(hsoListGVK)
	if err := d.Client.List(ctx, list, client.Limit(1)); err != nil {
		if meta.IsNoMatchError(err) {
			logger.V(1).Info("KEDA HTTPScaledObject CRD not found, skipping KEDA integration")
			return nil
		}
		return fmt.Errorf("failed to list HTTPScaledObjects: %w", err)
	}

	targetNS := env.Namespace
	if env.Spec.Deploy.Namespace == "create" {
		targetNS = env.PreviewNamespace()
	}

	targetName := env.Name
	targetPort := int64(80)
	if env.Spec.ServiceConfig != nil {
		if env.Spec.ServiceConfig.ServiceName != "" {
			targetName = fmt.Sprintf("%s-%s", env.Name, env.Spec.ServiceConfig.ServiceName)
		}
		if env.Spec.ServiceConfig.Port != 0 {
			targetPort = int64(env.Spec.ServiceConfig.Port)
		}
	}
	targetName = boundK8sName(targetName)

	// S1: Check if KEDA is explicitly disabled via CRD
	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.KEDA != nil {
		k := env.Spec.ServiceConfig.KEDA
		if k.Enabled != nil && !*k.Enabled {
			logger.Info("KEDA explicitly disabled for service, cleaning up any existing HSO",
				"name", targetName, "namespace", targetNS)
			return d.deleteHSOIfExists(ctx, targetName, targetNS)
		}
	}

	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(hsoGVK)
	hso.SetName(targetName)
	hso.SetNamespace(targetNS)
	hso.SetLabels(map[string]string{
		"divergedev.com/managed-by":  "diverge",
		"divergedev.com/environment": env.Name,
	})

	if isArgoRolloutsDeployer(d.Inner) {
		if err := unstructured.SetNestedField(hso.Object, "argoproj.io/v1alpha1", "spec", "scaleTargetRef", "apiVersion"); err != nil {
			return fmt.Errorf("failed to set scaleTargetRef.apiVersion: %w", err)
		}
		if err := unstructured.SetNestedField(hso.Object, "Rollout", "spec", "scaleTargetRef", "kind"); err != nil {
			return fmt.Errorf("failed to set scaleTargetRef.kind: %w", err)
		}
	}
	if err := unstructured.SetNestedField(hso.Object, targetName, "spec", "scaleTargetRef", "name"); err != nil {
		return fmt.Errorf("failed to set scaleTargetRef.name: %w", err)
	}
	if err := unstructured.SetNestedField(hso.Object, targetName, "spec", "scaleTargetRef", "service"); err != nil {
		return fmt.Errorf("failed to set scaleTargetRef.service: %w", err)
	}
	if err := unstructured.SetNestedField(hso.Object, targetPort, "spec", "scaleTargetRef", "port"); err != nil {
		return fmt.Errorf("failed to set scaleTargetRef.port: %w", err)
	}

	// Use pre-validated values from resolveKEDAConfig
	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.KEDA != nil {
		logger.Info("CRD KEDA config overrides applied",
			"minReplicas", minRepl, "maxReplicas", maxRepl, "cooldown", cooldown)
	}

	if err := unstructured.SetNestedField(hso.Object, minRepl, "spec", "replicas", "min"); err != nil {
		return fmt.Errorf("failed to set replicas.min: %w", err)
	}
	if err := unstructured.SetNestedField(hso.Object, maxRepl, "spec", "replicas", "max"); err != nil {
		return fmt.Errorf("failed to set replicas.max: %w", err)
	}
	if err := unstructured.SetNestedField(hso.Object, cooldown, "spec", "scaledownPeriod"); err != nil {
		return fmt.Errorf("failed to set scaledownPeriod: %w", err)
	}

	// Apply via Server-Side Apply (create-or-patch)
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(hsoGVK)
	err := d.Client.Get(ctx, client.ObjectKey{Name: targetName, Namespace: targetNS}, existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to check existing HTTPScaledObject: %w", err)
		}
		// Resource doesn't exist, create it
		if err := d.Client.Create(ctx, hso); err != nil {
			return fmt.Errorf("failed to create HTTPScaledObject: %w", err)
		}
	} else {
		// Resource exists, patch via SSA
		opts := []client.ApplyOption{
			client.FieldOwner("diverge-keda-deployer"),
			client.ForceOwnership,
		}
		if err := d.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(hso), opts...); err != nil {
			return fmt.Errorf("failed to apply HTTPScaledObject: %w", err)
		}
	}

	logger.Info("Successfully deployed HTTPScaledObject", "name", targetName, "namespace", targetNS)
	return nil
}

// deleteHSOIfExists removes an HTTPScaledObject if it exists, used when KEDA is explicitly disabled.
func (d *KEDADeployer) deleteHSOIfExists(ctx context.Context, name, namespace string) error {
	deleteCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(hsoGVK)
	hso.SetName(name)
	hso.SetNamespace(namespace)

	if err := d.Client.Delete(deleteCtx, hso); err != nil {
		if !meta.IsNoMatchError(err) && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete HTTPScaledObject for disabled KEDA: %w", err)
		}
	}
	return nil
}

// resolveKEDAConfig resolves KEDA configuration using precedence: CRD > CLI > built-in defaults.
func (d *KEDADeployer) resolveKEDAConfig(env *v1alpha1.Environment) (minRepl, maxRepl, cooldown int64) {
	minRepl = d.Config.MinReplicas
	maxRepl = d.Config.MaxReplicas
	cooldown = d.Config.Cooldown
	maxReplOverridden := false
	cooldownOverridden := false

	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.KEDA != nil {
		k := env.Spec.ServiceConfig.KEDA
		if k.MinReplicas != nil && *k.MinReplicas >= 0 {
			minRepl = int64(*k.MinReplicas)
		}
		if k.MaxReplicas != nil && *k.MaxReplicas >= 1 {
			maxRepl = int64(*k.MaxReplicas)
			maxReplOverridden = true
		}
		if k.CooldownPeriod != nil && *k.CooldownPeriod >= 0 {
			cooldown = int64(*k.CooldownPeriod)
			cooldownOverridden = true
		}
	}

	if maxRepl == 0 && !maxReplOverridden {
		maxRepl = 3
	}
	if cooldown == 0 && !cooldownOverridden {
		cooldown = 300
	}
	return
}

// Teardown performs its designated operation.
func (d *KEDADeployer) Teardown(ctx context.Context, env *v1alpha1.Environment) error {
	logger := log.FromContext(ctx).WithName("keda-deployer")

	targetNS := env.Namespace
	if env.Spec.Deploy.Namespace == "create" {
		targetNS = env.PreviewNamespace()
	}

	targetName := env.Name
	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.ServiceName != "" {
		targetName = fmt.Sprintf("%s-%s", env.Name, env.Spec.ServiceConfig.ServiceName)
	}
	targetName = boundK8sName(targetName)

	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(hsoGVK)
	hso.SetName(targetName)
	hso.SetNamespace(targetNS)

	if err := d.Client.Delete(ctx, hso); err != nil {
		if !meta.IsNoMatchError(err) && client.IgnoreNotFound(err) != nil {
			return fmt.Errorf("failed to delete HTTPScaledObject: %w", err)
		}
		logger.V(1).Info("HTTPScaledObject CRD missing or resource not found during teardown")
	}

	return d.Inner.Teardown(ctx, env)
}

// Status performs its designated operation.
func (d *KEDADeployer) Status(ctx context.Context, env *v1alpha1.Environment) ([]ServiceStatus, error) {
	innerStatus, err := d.Inner.Status(ctx, env)
	if err != nil {
		return nil, err
	}

	targetNS := env.Namespace
	if env.Spec.Deploy.Namespace == "create" {
		targetNS = env.PreviewNamespace()
	}

	targetName := env.Name
	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.ServiceName != "" {
		targetName = fmt.Sprintf("%s-%s", env.Name, env.Spec.ServiceConfig.ServiceName)
	}
	targetName = boundK8sName(targetName)

	hso := &unstructured.Unstructured{}
	hso.SetGroupVersionKind(hsoGVK)
	err = d.Client.Get(ctx, client.ObjectKey{Name: targetName, Namespace: targetNS}, hso)
	if err != nil {
		if meta.IsNoMatchError(err) || client.IgnoreNotFound(err) == nil {
			// HSO doesn't exist or CRD doesn't exist, we just return inner status
			return innerStatus, nil
		}
		return nil, fmt.Errorf("failed to get HTTPScaledObject: %w", err)
	}

	// Aggregate status from both Inner deployer and HTTPScaledObject.
	// If HSO exists, append its status.
	hsoReady := isHSOReady(hso)
	hsoHealth := "Progressing"
	if hsoReady {
		hsoHealth = "Healthy"
	}

	innerStatus = append(innerStatus, ServiceStatus{
		Name:       env.Name,
		Service:    "http-scaled-object",
		SyncStatus: "Applied",
		Health:     hsoHealth,
	})

	return innerStatus, nil
}

// isHSOReady reports whether an HTTPScaledObject has Ready=True in its status conditions.
func isHSOReady(hso *unstructured.Unstructured) bool {
	conditions, found, err := unstructured.NestedSlice(hso.Object, "status", "conditions")
	if !found || err != nil {
		return false
	}
	for _, cond := range conditions {
		if c, ok := cond.(map[string]interface{}); ok {
			if c["type"] == "Ready" && c["status"] == "True" {
				return true
			}
		}
	}
	return false
}

// isArgoRolloutsDeployer reports whether dep is an ArgoRolloutsDeployer or wraps one.
func isArgoRolloutsDeployer(dep Deployer) bool {
	for dep != nil {
		switch inner := dep.(type) {
		case *ArgoRolloutsDeployer:
			return true
		case *InstrumentedDeployer:
			dep = inner.Inner
		case *KEDADeployer:
			dep = inner.Inner
		default:
			return false
		}
	}
	return false
}

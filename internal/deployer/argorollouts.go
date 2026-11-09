package deployer

import (
	"context"
	"crypto/sha256"
	"fmt"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/registry"
)

const (
	// AnnotationRolloutAnalysis enables automatic generation of an Argo Rollouts AnalysisTemplate
	// that runs diverge verify (and optionally diverge replay) against the candidate preview revision.
	AnnotationRolloutAnalysis = "divergedev.com/rollout-analysis"
	// AnnotationRolloutAnalysisTemplate references an existing AnalysisTemplate by name in the target namespace.
	AnnotationRolloutAnalysisTemplate = "divergedev.com/rollout-analysis-template"
	// AnnotationRolloutVerifyPath configures the HTTP path probed by the generated AnalysisTemplate (default "/").
	AnnotationRolloutVerifyPath = "divergedev.com/rollout-verify-path"
	// AnnotationRolloutReplayTrace configures a trace file path for diverge replay inside the generated AnalysisTemplate.
	AnnotationRolloutReplayTrace = "divergedev.com/rollout-replay-trace"
	// AnnotationRolloutVerifyImage overrides the container image used by the generated AnalysisTemplate Job (default "ghcr.io/divergedev/diverge-cli:latest").
	AnnotationRolloutVerifyImage = "divergedev.com/rollout-verify-image"
	// AnnotationRolloutServiceAccount overrides the ServiceAccount used by the generated AnalysisTemplate Job (default "diverge-controller").
	AnnotationRolloutServiceAccount = "divergedev.com/rollout-service-account"

	defaultRolloutVerifyImage    = "ghcr.io/divergedev/diverge-cli:latest"
	defaultRolloutServiceAccount = "diverge-controller"
)

var (
	rolloutGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "Rollout",
	}
	analysisTemplateGVK = schema.GroupVersionKind{
		Group:   "argoproj.io",
		Version: "v1alpha1",
		Kind:    "AnalysisTemplate",
	}
	coreServiceGVK = schema.GroupVersionKind{
		Group:   "",
		Version: "v1",
		Kind:    "Service",
	}
)

func init() {
	provider := registry.Provider[Deployer]{
		Create: func(deps registry.Deps) (Deployer, error) {
			return &ArgoRolloutsDeployer{
				Client: deps.Client,
			}, nil
		},
		Description: "Argo Rollouts progressive delivery with optional AnalysisTemplate verification",
	}
	Providers.Register("argo-rollouts", provider)
	Providers.Register("argorollouts", provider)
}

// ArgoRolloutsDeployer deploys preview workloads as Argo Rollouts (`argoproj.io/v1alpha1` Rollout)
// with an accompanying Kubernetes Service and optional pre-promotion AnalysisTemplate.
type ArgoRolloutsDeployer struct {
	Client client.Client
}

var _ Deployer = (*ArgoRolloutsDeployer)(nil)

// boundK8sName bounds a Kubernetes resource name to at most 63 characters using a deterministic SHA-256 suffix when truncated.
func boundK8sName(raw string) string {
	raw = strings.Trim(raw, "-")
	if len(raw) <= 63 {
		return raw
	}
	hash := fmt.Sprintf("%x", sha256.Sum256([]byte(raw)))[:8]
	prefix := strings.TrimRight(raw[:63-9], "-")
	if prefix == "" {
		prefix = "r"
	}
	return prefix + "-" + hash
}

// resolveTarget computes the target namespace, Rollout/Service name, baseline service name, container image, and port for an Environment.
func (d *ArgoRolloutsDeployer) resolveTarget(env *v1alpha1.Environment) (targetNS, targetName, serviceName, image string, port int64) {
	targetNS = env.Namespace
	if env.Spec.Deploy.Namespace == "create" {
		targetNS = env.PreviewNamespace()
	}
	targetName = boundK8sName(env.Name)
	serviceName = env.Name
	port = 80
	if cfg := env.Spec.ServiceConfig; cfg != nil {
		if cfg.ServiceName != "" {
			serviceName = cfg.ServiceName
			targetName = boundK8sName(fmt.Sprintf("%s-%s", env.Name, cfg.ServiceName))
		}
		if cfg.Port > 0 {
			port = int64(cfg.Port)
		}
		image = cfg.Image
	}
	return targetNS, targetName, serviceName, image, port
}

// resolveStableServiceName returns a distinct Kubernetes Service name for the Rollout's stableService field.
func (d *ArgoRolloutsDeployer) resolveStableServiceName(targetName, serviceName string) (stableSvcName string, managedByRollout bool) {
	if serviceName != "" && serviceName != targetName {
		return boundK8sName(serviceName), false
	}
	return boundK8sName(targetName + "-stable"), true
}

// setMetadataAndOwner populates standard Diverge metadata labels and same-namespace OwnerReferences on a resource.
func (d *ArgoRolloutsDeployer) setMetadataAndOwner(obj *unstructured.Unstructured, env *v1alpha1.Environment, targetNS, name, serviceName string) {
	obj.SetName(name)
	obj.SetNamespace(targetNS)
	obj.SetLabels(map[string]string{
		"app":                        name,
		"divergedev.com/managed-by":  "diverge",
		"divergedev.com/environment": env.Name,
		"divergedev.com/service":     serviceName,
	})
	if env.Spec.Deploy.Namespace != "create" && targetNS == env.Namespace && env.UID != "" {
		apiVersion := env.APIVersion
		if apiVersion == "" {
			apiVersion = "divergedev.com/v1alpha1"
		}
		kind := env.Kind
		if kind == "" {
			kind = "Environment"
		}
		obj.SetOwnerReferences([]metav1.OwnerReference{
			{
				APIVersion: apiVersion,
				Kind:       kind,
				Name:       env.Name,
				UID:        env.UID,
			},
		})
	}
}

// applyOrCreate creates the resource if it does not exist or patches it via Server-Side Apply if it already exists.
func (d *ArgoRolloutsDeployer) applyOrCreate(ctx context.Context, gvk schema.GroupVersionKind, obj *unstructured.Unstructured) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err := d.Client.Get(ctx, client.ObjectKey{Name: obj.GetName(), Namespace: obj.GetNamespace()}, existing)
	if err != nil {
		if client.IgnoreNotFound(err) != nil {
			return err
		}
		return d.Client.Create(ctx, obj)
	}
	return d.Client.Apply(ctx, client.ApplyConfigurationFromUnstructured(obj), client.FieldOwner("diverge"), client.ForceOwnership)
}

// ignoreNotFoundOrNoMatch ignores NotFound, NoMatch, and NotRegistered errors during cleanup operations.
func ignoreNotFoundOrNoMatch(err error) error {
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		return nil
	}
	return err
}

// deleteManagedResource deletes a Kubernetes resource only if its labels confirm it is managed by Diverge for envName.
func (d *ArgoRolloutsDeployer) deleteManagedResource(ctx context.Context, gvk schema.GroupVersionKind, targetNS, name, envName string) error {
	existing := &unstructured.Unstructured{}
	existing.SetGroupVersionKind(gvk)
	err := d.Client.Get(ctx, client.ObjectKey{Name: name, Namespace: targetNS}, existing)
	if err != nil {
		return ignoreNotFoundOrNoMatch(err)
	}
	labels := existing.GetLabels()
	if labels["divergedev.com/managed-by"] != "diverge" || labels["divergedev.com/environment"] != envName {
		return nil
	}
	return ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, existing))
}

// buildService constructs a Kubernetes Service selecting the Rollout's pods.
func (d *ArgoRolloutsDeployer) buildService(env *v1alpha1.Environment, targetNS, svcName, selectorApp, serviceName string, port int64) (*unstructured.Unstructured, error) {
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(coreServiceGVK)
	d.setMetadataAndOwner(svc, env, targetNS, svcName, serviceName)
	if err := unstructured.SetNestedMap(svc.Object, map[string]interface{}{
		"app":                        selectorApp,
		"divergedev.com/environment": env.Name,
	}, "spec", "selector"); err != nil {
		return nil, fmt.Errorf("failed to set service selector: %w", err)
	}
	if err := unstructured.SetNestedSlice(svc.Object, []interface{}{
		map[string]interface{}{
			"name":       "http",
			"port":       port,
			"targetPort": port,
			"protocol":   "TCP",
		},
	}, "spec", "ports"); err != nil {
		return nil, fmt.Errorf("failed to set service ports: %w", err)
	}
	return svc, nil
}

// Deploy creates or updates the Kubernetes Service, optional AnalysisTemplate, and Argo Rollout for the environment.
func (d *ArgoRolloutsDeployer) Deploy(ctx context.Context, env *v1alpha1.Environment) error {
	targetNS, targetName, serviceName, image, port := d.resolveTarget(env)

	// 1. Reconcile the backing Kubernetes Service so Gateway API / Istio / Activator Proxy can route to it.
	svc, err := d.buildService(env, targetNS, targetName, targetName, serviceName, port)
	if err != nil {
		return err
	}
	if err := d.applyOrCreate(ctx, coreServiceGVK, svc); err != nil {
		return fmt.Errorf("failed to apply rollout service: %w", err)
	}

	// Reconcile a dedicated stable Service when targetName and serviceName are identical.
	stableSvcName, manageStableSvc := d.resolveStableServiceName(targetName, serviceName)
	if manageStableSvc {
		stableSvc, err := d.buildService(env, targetNS, stableSvcName, targetName, serviceName, port)
		if err != nil {
			return err
		}
		if err := d.applyOrCreate(ctx, coreServiceGVK, stableSvc); err != nil {
			return fmt.Errorf("failed to apply rollout stable service: %w", err)
		}
	}

	// 2. Reconcile optional AnalysisTemplate when canary verification/replay analysis is requested.
	generatedTemplateName := boundK8sName(targetName + "-verify")
	analysisTemplateName := strings.TrimSpace(env.Annotations[AnnotationRolloutAnalysisTemplate])
	shouldGenerateAnalysis := strings.EqualFold(strings.TrimSpace(env.Annotations[AnnotationRolloutAnalysis]), "true") ||
		strings.TrimSpace(env.Annotations[AnnotationRolloutVerifyPath]) != "" ||
		strings.TrimSpace(env.Annotations[AnnotationRolloutReplayTrace]) != ""

	if shouldGenerateAnalysis {
		tmpl := d.buildAnalysisTemplate(env, targetNS, generatedTemplateName, serviceName)
		if err := d.applyOrCreate(ctx, analysisTemplateGVK, tmpl); err != nil {
			return fmt.Errorf("failed to apply analysis template: %w", err)
		}
		if analysisTemplateName == "" {
			analysisTemplateName = generatedTemplateName
		}
	} else if analysisTemplateName != generatedTemplateName {
		if err := d.deleteManagedResource(ctx, analysisTemplateGVK, targetNS, generatedTemplateName, env.Name); err != nil {
			return fmt.Errorf("failed to delete stale analysis template: %w", err)
		}
	}

	// 3. Reconcile the Argo Rollout resource.
	rollout := d.buildRollout(env, targetNS, targetName, stableSvcName, serviceName, image, port, analysisTemplateName)
	if err := d.applyOrCreate(ctx, rolloutGVK, rollout); err != nil {
		return fmt.Errorf("failed to apply argo rollout: %w", err)
	}

	return nil
}

// buildAnalysisTemplate constructs the pre-promotion Argo Rollouts AnalysisTemplate for an environment.
func (d *ArgoRolloutsDeployer) buildAnalysisTemplate(env *v1alpha1.Environment, targetNS, templateName, serviceName string) *unstructured.Unstructured {
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(analysisTemplateGVK)
	d.setMetadataAndOwner(tmpl, env, targetNS, templateName, serviceName)

	verifyImg := strings.TrimSpace(env.Annotations[AnnotationRolloutVerifyImage])
	if verifyImg == "" {
		verifyImg = defaultRolloutVerifyImage
	}
	verifyPath := strings.TrimSpace(env.Annotations[AnnotationRolloutVerifyPath])
	if verifyPath == "" {
		verifyPath = "/"
	}
	replayTrace := strings.TrimSpace(env.Annotations[AnnotationRolloutReplayTrace])
	serviceAccount := strings.TrimSpace(env.Annotations[AnnotationRolloutServiceAccount])
	if serviceAccount == "" {
		serviceAccount = defaultRolloutServiceAccount
	}

	var args []interface{}
	if replayTrace != "" {
		args = []interface{}{
			"replay",
			env.Name,
			"-n",
			env.Namespace,
			"--from",
			replayTrace,
		}
	} else {
		args = []interface{}{
			"verify",
			env.Name,
			"-n",
			env.Namespace,
			"--path",
			verifyPath,
			"--compare-baseline",
		}
	}

	_ = unstructured.SetNestedSlice(tmpl.Object, []interface{}{
		map[string]interface{}{
			"name": "diverge-preview-analysis",
			"provider": map[string]interface{}{
				"job": map[string]interface{}{
					"spec": map[string]interface{}{
						"backoffLimit": int64(1),
						"template": map[string]interface{}{
							"spec": map[string]interface{}{
								"serviceAccountName": serviceAccount,
								"restartPolicy":      "Never",
								"containers": []interface{}{
									map[string]interface{}{
										"name":  "diverge-verify",
										"image": verifyImg,
										"args":  args,
									},
								},
							},
						},
					},
				},
			},
		},
	}, "spec", "metrics")

	return tmpl
}

// buildRollout constructs the Argo Rollouts Rollout resource for an environment.
func (d *ArgoRolloutsDeployer) buildRollout(env *v1alpha1.Environment, targetNS, targetName, stableSvcName, serviceName, image string, port int64, analysisTemplateName string) *unstructured.Unstructured {
	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	d.setMetadataAndOwner(rollout, env, targetNS, targetName, serviceName)

	replicas := int64(1)
	if strings.EqualFold(strings.TrimSpace(env.Annotations["divergedev.com/hibernated"]), "true") {
		replicas = 0
	}
	_ = unstructured.SetNestedField(rollout.Object, replicas, "spec", "replicas")

	podLabels := map[string]interface{}{
		"app":                        targetName,
		"divergedev.com/managed-by":  "diverge",
		"divergedev.com/environment": env.Name,
		"divergedev.com/service":     serviceName,
	}
	_ = unstructured.SetNestedMap(rollout.Object, map[string]interface{}{
		"app":                        targetName,
		"divergedev.com/environment": env.Name,
	}, "spec", "selector", "matchLabels")
	_ = unstructured.SetNestedMap(rollout.Object, podLabels, "spec", "template", "metadata", "labels")

	podAnnotations := make(map[string]string)
	for k, v := range env.Annotations {
		if strings.HasPrefix(k, "instrumentation.opentelemetry.io/") {
			podAnnotations[k] = v
		}
	}
	if fp := strings.TrimSpace(env.Annotations["divergedev.com/baseline-fingerprint"]); fp != "" {
		podAnnotations["divergedev.com/baseline-fingerprint"] = fp
	}
	if len(podAnnotations) > 0 {
		_ = unstructured.SetNestedStringMap(rollout.Object, podAnnotations, "spec", "template", "metadata", "annotations")
	}

	pullPolicy := "IfNotPresent"
	if env.Spec.ServiceConfig != nil && strings.TrimSpace(env.Spec.ServiceConfig.ImagePullPolicy) != "" {
		pullPolicy = strings.TrimSpace(env.Spec.ServiceConfig.ImagePullPolicy)
	}

	var containerEnv []interface{}
	seenEnv := map[string]bool{"APP_VERSION": true}
	containerEnv = append(containerEnv, map[string]interface{}{
		"name":  "APP_VERSION",
		"value": env.Name,
	})
	if env.Spec.ServiceConfig != nil {
		for _, e := range env.Spec.ServiceConfig.Env {
			if seenEnv[e.Name] {
				continue
			}
			seenEnv[e.Name] = true
			containerEnv = append(containerEnv, map[string]interface{}{
				"name":  e.Name,
				"value": e.Value,
			})
		}
	}

	var containerEnvFrom []interface{}
	seenSecrets := make(map[string]bool)
	addSecretEnvFrom := func(secretName string) {
		secretName = strings.TrimSpace(secretName)
		if secretName == "" || seenSecrets[secretName] {
			return
		}
		seenSecrets[secretName] = true
		containerEnvFrom = append(containerEnvFrom, map[string]interface{}{
			"secretRef": map[string]interface{}{
				"name": secretName,
			},
		})
	}

	if dbSecret := resolveDBSecret(env); dbSecret != "" {
		addSecretEnvFrom(dbSecret)
		if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.DatabaseEnvKey != "" && !seenEnv[env.Spec.ServiceConfig.DatabaseEnvKey] {
			seenEnv[env.Spec.ServiceConfig.DatabaseEnvKey] = true
			containerEnv = append(containerEnv, map[string]interface{}{
				"name": env.Spec.ServiceConfig.DatabaseEnvKey,
				"valueFrom": map[string]interface{}{
					"secretKeyRef": map[string]interface{}{
						"name": dbSecret,
						"key":  "DATABASE_URL",
					},
				},
			})
		}
	}

	for _, ref := range append(append([]v1alpha1.SecretRef(nil), env.Spec.Deploy.EnvFrom...), env.Spec.EnvFrom...) {
		secName := strings.TrimSpace(ref.Name)
		if secName == "" {
			continue
		}
		key := strings.TrimSpace(ref.Key)
		if key != "" {
			if !seenEnv[key] {
				seenEnv[key] = true
				containerEnv = append(containerEnv, map[string]interface{}{
					"name": key,
					"valueFrom": map[string]interface{}{
						"secretKeyRef": map[string]interface{}{
							"name": secName,
							"key":  key,
						},
					},
				})
			}
		} else {
			addSecretEnvFrom(secName)
		}
	}

	containerSpec := map[string]interface{}{
		"name":            serviceName,
		"image":           image,
		"imagePullPolicy": pullPolicy,
		"env":             containerEnv,
		"ports": []interface{}{
			map[string]interface{}{
				"name":          "http",
				"containerPort": port,
				"protocol":      "TCP",
			},
		},
	}
	if len(containerEnvFrom) > 0 {
		containerSpec["envFrom"] = containerEnvFrom
	}

	if env.Spec.ServiceConfig != nil && env.Spec.ServiceConfig.Resources != nil {
		resCfg := env.Spec.ServiceConfig.Resources
		requests := make(map[string]interface{})
		limits := make(map[string]interface{})
		if cpuReq := strings.TrimSpace(resCfg.CPURequest); cpuReq != "" {
			requests["cpu"] = cpuReq
		}
		if memReq := strings.TrimSpace(resCfg.MemoryRequest); memReq != "" {
			requests["memory"] = memReq
		}
		if cpuLim := strings.TrimSpace(resCfg.CPULimit); cpuLim != "" {
			limits["cpu"] = cpuLim
		}
		if memLim := strings.TrimSpace(resCfg.MemoryLimit); memLim != "" {
			limits["memory"] = memLim
		}
		resources := make(map[string]interface{})
		if len(requests) > 0 {
			resources["requests"] = requests
		}
		if len(limits) > 0 {
			resources["limits"] = limits
		}
		if len(resources) > 0 {
			containerSpec["resources"] = resources
		}
	}

	_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{containerSpec}, "spec", "template", "spec", "containers")

	canarySpec := map[string]interface{}{
		"canaryService": targetName,
		"stableService": stableSvcName,
	}
	if analysisTemplateName != "" {
		canarySpec["analysis"] = map[string]interface{}{
			"templates": []interface{}{
				map[string]interface{}{
					"templateName": analysisTemplateName,
				},
			},
		}
	}
	_ = unstructured.SetNestedMap(rollout.Object, canarySpec, "spec", "strategy", "canary")

	return rollout
}

// Teardown deletes the Argo Rollout, generated AnalysisTemplate, and Service for the environment.
func (d *ArgoRolloutsDeployer) Teardown(ctx context.Context, env *v1alpha1.Environment) error {
	targetNS, targetName, serviceName, _, _ := d.resolveTarget(env)

	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetName(targetName)
	rollout.SetNamespace(targetNS)
	if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, rollout)); err != nil {
		return fmt.Errorf("failed to delete argo rollout: %w", err)
	}

	generatedTemplateName := boundK8sName(targetName + "-verify")
	if err := d.deleteManagedResource(ctx, analysisTemplateGVK, targetNS, generatedTemplateName, env.Name); err != nil {
		return fmt.Errorf("failed to delete analysis template: %w", err)
	}

	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(coreServiceGVK)
	svc.SetName(targetName)
	svc.SetNamespace(targetNS)
	if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, svc)); err != nil {
		return fmt.Errorf("failed to delete rollout service: %w", err)
	}

	stableSvcName, manageStableSvc := d.resolveStableServiceName(targetName, serviceName)
	if manageStableSvc {
		if err := d.deleteManagedResource(ctx, coreServiceGVK, targetNS, stableSvcName, env.Name); err != nil {
			return fmt.Errorf("failed to delete rollout stable service: %w", err)
		}
	}

	return nil
}

// Status returns the current deployment health of the Argo Rollout for the environment.
func (d *ArgoRolloutsDeployer) Status(ctx context.Context, env *v1alpha1.Environment) ([]ServiceStatus, error) {
	targetNS, targetName, serviceName, _, _ := d.resolveTarget(env)

	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	err := d.Client.Get(ctx, client.ObjectKey{Name: targetName, Namespace: targetNS}, rollout)

	status := ServiceStatus{
		Name:       targetName,
		Service:    serviceName,
		SyncStatus: "Applied",
	}

	if err != nil {
		if apierrors.IsNotFound(err) || meta.IsNoMatchError(err) {
			if !env.DeletionTimestamp.IsZero() {
				return []ServiceStatus{}, nil
			}
			status.Health = "Missing"
			return []ServiceStatus{status}, nil
		}
		return nil, fmt.Errorf("failed to get argo rollout: %w", err)
	}

	health := "Progressing"
	gen := rollout.GetGeneration()
	obsGen, hasObsGen, _ := unstructured.NestedInt64(rollout.Object, "status", "observedGeneration")
	if gen > 0 && hasObsGen && obsGen < gen {
		health = "Progressing"
	} else if phase, found, _ := unstructured.NestedString(rollout.Object, "status", "phase"); found && phase != "" {
		switch phase {
		case "Healthy":
			health = "Healthy"
		case "Degraded":
			health = "Degraded"
		case "Paused", "Progressing":
			health = "Progressing"
		}
	} else if conditions, found, _ := unstructured.NestedSlice(rollout.Object, "status", "conditions"); found {
		for _, c := range conditions {
			cond, ok := c.(map[string]interface{})
			if !ok {
				continue
			}
			t, _ := cond["type"].(string)
			s, _ := cond["status"].(string)
			if (t == "Degraded" && s == "True") || (t == "Progressing" && s == "False") {
				health = "Degraded"
				break
			}
			if (t == "Healthy" || t == "Completed") && s == "True" {
				health = "Healthy"
			}
		}
	}

	status.Health = health
	if msg, found, _ := unstructured.NestedString(rollout.Object, "status", "message"); found && msg != "" {
		status.Message = msg
	}
	return []ServiceStatus{status}, nil
}

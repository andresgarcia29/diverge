package deployer

import (
	"context"
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
	// AnnotationRolloutVerifyImage overrides the container image used by the generated AnalysisTemplate Job (default "ghcr.io/divergedev/diverge:latest").
	AnnotationRolloutVerifyImage = "divergedev.com/rollout-verify-image"

	defaultRolloutVerifyImage = "ghcr.io/divergedev/diverge:latest"
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

func (d *ArgoRolloutsDeployer) resolveTarget(env *v1alpha1.Environment) (targetNS, targetName, serviceName, image string, port int64) {
	targetNS = env.Namespace
	if env.Spec.Deploy.Namespace == "create" {
		targetNS = env.PreviewNamespace()
	}
	targetName = env.Name
	serviceName = env.Name
	port = 80
	if cfg := env.Spec.ServiceConfig; cfg != nil {
		if cfg.ServiceName != "" {
			serviceName = cfg.ServiceName
			targetName = fmt.Sprintf("%s-%s", env.Name, cfg.ServiceName)
		}
		if cfg.Port > 0 {
			port = int64(cfg.Port)
		}
		image = cfg.Image
	}
	return targetNS, targetName, serviceName, image, port
}

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

func ignoreNotFoundOrNoMatch(err error) error {
	if err == nil || apierrors.IsNotFound(err) || meta.IsNoMatchError(err) || runtime.IsNotRegisteredError(err) {
		return nil
	}
	return err
}

// Deploy creates or updates the Kubernetes Service, optional AnalysisTemplate, and Argo Rollout for the environment.
func (d *ArgoRolloutsDeployer) Deploy(ctx context.Context, env *v1alpha1.Environment) error {
	targetNS, targetName, serviceName, image, port := d.resolveTarget(env)

	// 1. Reconcile the backing Kubernetes Service so Gateway API / Istio / Activator Proxy can route to it.
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(coreServiceGVK)
	d.setMetadataAndOwner(svc, env, targetNS, targetName, serviceName)
	if err := unstructured.SetNestedMap(svc.Object, map[string]interface{}{
		"app":                        targetName,
		"divergedev.com/environment": env.Name,
	}, "spec", "selector"); err != nil {
		return fmt.Errorf("failed to set service selector: %w", err)
	}
	if err := unstructured.SetNestedSlice(svc.Object, []interface{}{
		map[string]interface{}{
			"name":       "http",
			"port":       port,
			"targetPort": port,
			"protocol":   "TCP",
		},
	}, "spec", "ports"); err != nil {
		return fmt.Errorf("failed to set service ports: %w", err)
	}
	if err := d.applyOrCreate(ctx, coreServiceGVK, svc); err != nil {
		return fmt.Errorf("failed to apply rollout service: %w", err)
	}

	// 2. Reconcile optional AnalysisTemplate when canary verification/replay analysis is requested.
	generatedTemplateName := targetName + "-verify"
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
	} else {
		stale := &unstructured.Unstructured{}
		stale.SetGroupVersionKind(analysisTemplateGVK)
		stale.SetName(generatedTemplateName)
		stale.SetNamespace(targetNS)
		if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, stale)); err != nil {
			return fmt.Errorf("failed to delete stale analysis template: %w", err)
		}
	}

	// 3. Reconcile the Argo Rollout resource.
	rollout := d.buildRollout(env, targetNS, targetName, serviceName, image, port, analysisTemplateName)
	if err := d.applyOrCreate(ctx, rolloutGVK, rollout); err != nil {
		return fmt.Errorf("failed to apply argo rollout: %w", err)
	}

	return nil
}

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
								"restartPolicy": "Never",
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

func (d *ArgoRolloutsDeployer) buildRollout(env *v1alpha1.Environment, targetNS, targetName, serviceName, image string, port int64, analysisTemplateName string) *unstructured.Unstructured {
	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	d.setMetadataAndOwner(rollout, env, targetNS, targetName, serviceName)

	replicas := int64(1)
	if env.Annotations["divergedev.com/hibernated"] == "true" {
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
	if len(podAnnotations) > 0 {
		_ = unstructured.SetNestedStringMap(rollout.Object, podAnnotations, "spec", "template", "metadata", "annotations")
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

	containerSpec := map[string]interface{}{
		"name":  serviceName,
		"image": image,
		"env":   containerEnv,
		"ports": []interface{}{
			map[string]interface{}{
				"name":          "http",
				"containerPort": port,
				"protocol":      "TCP",
			},
		},
	}
	_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{containerSpec}, "spec", "template", "spec", "containers")

	canarySpec := map[string]interface{}{
		"canaryService": targetName,
		"stableService": targetName,
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
	targetNS, targetName, _, _, _ := d.resolveTarget(env)

	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	rollout.SetName(targetName)
	rollout.SetNamespace(targetNS)
	if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, rollout)); err != nil {
		return fmt.Errorf("failed to delete argo rollout: %w", err)
	}

	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(analysisTemplateGVK)
	tmpl.SetName(targetName + "-verify")
	tmpl.SetNamespace(targetNS)
	if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, tmpl)); err != nil {
		return fmt.Errorf("failed to delete analysis template: %w", err)
	}

	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(coreServiceGVK)
	svc.SetName(targetName)
	svc.SetNamespace(targetNS)
	if err := ignoreNotFoundOrNoMatch(d.Client.Delete(ctx, svc)); err != nil {
		return fmt.Errorf("failed to delete rollout service: %w", err)
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
			status.Health = "Missing"
			return []ServiceStatus{status}, nil
		}
		return nil, fmt.Errorf("failed to get argo rollout: %w", err)
	}

	health := "Progressing"
	if phase, found, _ := unstructured.NestedString(rollout.Object, "status", "phase"); found && phase != "" {
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
			if t == "Degraded" && s == "True" {
				health = "Degraded"
				break
			}
			if (t == "Healthy" || t == "Completed" || t == "Available") && s == "True" {
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

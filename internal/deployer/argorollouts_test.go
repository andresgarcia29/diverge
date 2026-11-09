package deployer

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/pkg/registry"
)

func TestArgoRolloutsDeployer_RegistryAndLifecycle(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewClientBuilder().Build()

	dep, err := Providers.Create("argo-rollouts", registry.Deps{Client: fakeClient})
	require.NoError(t, err)
	require.NotNil(t, dep)

	aliasDep, err := Providers.Create("argorollouts", registry.Deps{Client: fakeClient})
	require.NoError(t, err)
	require.NotNil(t, aliasDep)

	env := &v1alpha1.Environment{
		TypeMeta: metav1.TypeMeta{
			APIVersion: "divergedev.com/v1alpha1",
			Kind:       "Environment",
		},
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-42",
			Namespace: "default",
			UID:       "uid-pr-42",
			Annotations: map[string]string{
				AnnotationRolloutAnalysis:             "true",
				AnnotationRolloutVerifyPath:           "/api/v1/health",
				"instrumentation.opentelemetry.io/go": "true",
			},
		},
		Spec: v1alpha1.EnvironmentSpec{
			Deploy: v1alpha1.EnvironmentDeploy{
				Namespace: "same",
			},
			ServiceConfig: &v1alpha1.ServicePreviewConfig{
				ServiceName: "checkout",
				Image:       "ghcr.io/acme/checkout:pr-42",
				Port:        8080,
				Env: []v1alpha1.EnvVar{
					{Name: "LOG_LEVEL", Value: "debug"},
				},
			},
		},
	}

	require.NoError(t, dep.Deploy(ctx, env))

	// Verify Service was created
	svc := &unstructured.Unstructured{}
	svc.SetGroupVersionKind(coreServiceGVK)
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "pr-42-checkout", Namespace: "default"}, svc))
	assert.Equal(t, "pr-42", svc.GetLabels()["divergedev.com/environment"])

	// Verify AnalysisTemplate was created with diverge verify args
	tmpl := &unstructured.Unstructured{}
	tmpl.SetGroupVersionKind(analysisTemplateGVK)
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "pr-42-checkout-verify", Namespace: "default"}, tmpl))
	metrics, found, err := unstructured.NestedSlice(tmpl.Object, "spec", "metrics")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, metrics, 1)
	containers, found, err := unstructured.NestedSlice(metrics[0].(map[string]interface{}), "provider", "job", "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, containers, 1)
	assert.Equal(t, defaultRolloutVerifyImage, containers[0].(map[string]interface{})["image"])
	assert.Equal(t, []interface{}{"verify", "pr-42", "-n", "default", "--path", "/api/v1/health", "--compare-baseline"}, containers[0].(map[string]interface{})["args"])
	saName, found, err := unstructured.NestedString(metrics[0].(map[string]interface{}), "provider", "job", "spec", "template", "spec", "serviceAccountName")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, defaultRolloutServiceAccount, saName)

	// Verify Rollout was created with canary strategy and analysis template reference
	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "pr-42-checkout", Namespace: "default"}, rollout))
	assert.Len(t, rollout.GetOwnerReferences(), 1)
	canarySvc, found, err := unstructured.NestedString(rollout.Object, "spec", "strategy", "canary", "canaryService")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "pr-42-checkout", canarySvc)
	stableSvc, found, err := unstructured.NestedString(rollout.Object, "spec", "strategy", "canary", "stableService")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, "checkout", stableSvc)

	templates, found, err := unstructured.NestedSlice(rollout.Object, "spec", "strategy", "canary", "analysis", "templates")
	require.NoError(t, err)
	require.True(t, found)
	require.Len(t, templates, 1)
	assert.Equal(t, "pr-42-checkout-verify", templates[0].(map[string]interface{})["templateName"])

	// Status starts as Progressing when no status.phase is set
	statuses, err := dep.Status(ctx, env)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "Progressing", statuses[0].Health)

	// Simulate Argo Rollouts controller marking Rollout Healthy
	require.NoError(t, unstructured.SetNestedField(rollout.Object, "Healthy", "status", "phase"))
	require.NoError(t, unstructured.SetNestedField(rollout.Object, "Canary analysis passed", "status", "message"))
	require.NoError(t, fakeClient.Update(ctx, rollout))

	statuses, err = dep.Status(ctx, env)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "Healthy", statuses[0].Health)
	assert.Equal(t, "Canary analysis passed", statuses[0].Message)

	// Switching to replay trace annotation updates args and disabling analysis afterwards cleans up stale template
	env.Annotations = map[string]string{
		AnnotationRolloutReplayTrace: "./traces/checkout.jsonl",
	}
	require.NoError(t, dep.Deploy(ctx, env))
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "pr-42-checkout-verify", Namespace: "default"}, tmpl))
	metrics, found, err = unstructured.NestedSlice(tmpl.Object, "spec", "metrics")
	require.NoError(t, err)
	require.True(t, found)
	containers, found, err = unstructured.NestedSlice(metrics[0].(map[string]interface{}), "provider", "job", "spec", "template", "spec", "containers")
	require.NoError(t, err)
	require.True(t, found)
	assert.Equal(t, []interface{}{"replay", "pr-42", "-n", "default", "--from", "./traces/checkout.jsonl"}, containers[0].(map[string]interface{})["args"])

	env.Annotations = nil
	require.NoError(t, dep.Deploy(ctx, env))
	err = fakeClient.Get(ctx, client.ObjectKey{Name: "pr-42-checkout-verify", Namespace: "default"}, tmpl)
	assert.Error(t, err)

	// Teardown removes Rollout and Service
	require.NoError(t, dep.Teardown(ctx, env))
	statuses, err = dep.Status(ctx, env)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "Missing", statuses[0].Health)

	// When environment is terminating and Rollout is gone, Status returns empty slice so finalizer completes
	now := metav1.Now()
	env.DeletionTimestamp = &now
	statuses, err = dep.Status(ctx, env)
	require.NoError(t, err)
	assert.Empty(t, statuses)
}

func TestArgoRolloutsDeployer_ConditionsFallbackAndCreateNamespace(t *testing.T) {
	ctx := context.Background()
	fakeClient := fake.NewClientBuilder().Build()
	dep := &ArgoRolloutsDeployer{Client: fakeClient}

	env := &v1alpha1.Environment{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "pr-99",
			Namespace: "default",
			Annotations: map[string]string{
				"divergedev.com/hibernated": "true",
			},
		},
		Spec: v1alpha1.EnvironmentSpec{
			Deploy: v1alpha1.EnvironmentDeploy{
				Namespace: "create",
			},
			ServiceConfig: &v1alpha1.ServicePreviewConfig{
				Image: "ghcr.io/acme/api:pr-99",
			},
		},
	}

	require.NoError(t, dep.Deploy(ctx, env))

	targetNS := env.PreviewNamespace()
	rollout := &unstructured.Unstructured{}
	rollout.SetGroupVersionKind(rolloutGVK)
	require.NoError(t, fakeClient.Get(ctx, client.ObjectKey{Name: "pr-99", Namespace: targetNS}, rollout))
	assert.Empty(t, rollout.GetOwnerReferences())
	replicas, _, _ := unstructured.NestedInt64(rollout.Object, "spec", "replicas")
	assert.Equal(t, int64(0), replicas)

	// Simulate condition-based Degraded status
	_ = unstructured.SetNestedSlice(rollout.Object, []interface{}{
		map[string]interface{}{
			"type":   "Degraded",
			"status": "True",
		},
	}, "status", "conditions")
	require.NoError(t, fakeClient.Update(ctx, rollout))

	statuses, err := dep.Status(ctx, env)
	require.NoError(t, err)
	require.Len(t, statuses, 1)
	assert.Equal(t, "Degraded", statuses[0].Health)
}

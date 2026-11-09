package controller

import (
	"context"
	"fmt"
	"reflect"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
	"github.com/divergedev/diverge/internal/async"
	"github.com/divergedev/diverge/internal/changeset"
	"github.com/divergedev/diverge/internal/deployer"
	"github.com/divergedev/diverge/internal/events"
	"github.com/divergedev/diverge/internal/metrics"
	"github.com/divergedev/diverge/internal/notifier"
	"github.com/divergedev/diverge/internal/routing"
	divtesting "github.com/divergedev/diverge/internal/testing"
	"github.com/divergedev/diverge/pkg/database"
	"github.com/divergedev/diverge/pkg/features"
)

const environmentFinalizer = "divergedev.com/environment-protection"

// EnvironmentReconciler reconciles a Environment object
type EnvironmentReconciler struct {
	client.Client
	APIReader        client.Reader
	Scheme           *runtime.Scheme
	Recorder         *events.Recorder
	Router           routing.Router
	DatabaseProvider database.DatabaseProvider
	ChangeDetector   changeset.ChangeDetector
	Notifier         notifier.Notifier
	StatusReporter   notifier.StatusReporter
	Deployer         deployer.Deployer
	TestRunner       divtesting.TestRunner
	AsyncProvisioner async.Provisioner
	FeatureProvider  features.FeatureProvider
	SetupJobImage    string
	SetupRunner      SetupRunner
}

// +kubebuilder:rbac:groups=divergedev.com,resources=environments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=divergedev.com,resources=environments/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=divergedev.com,resources=environments/finalizers,verbs=update
// +kubebuilder:rbac:groups="",resources=namespaces;secrets;services;configmaps;events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=security.istio.io,resources=authorizationpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=gateway.networking.k8s.io,resources=gateways;httproutes;grpcroutes;referencegrants,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=cert-manager.io,resources=certificates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=argoproj.io,resources=applications;rollouts;analysistemplates,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=http.keda.sh,resources=httpscaledobjects,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=serving.knative.dev,resources=services,verbs=get;list;watch;create;update;patch;delete

// Reconcile performs its designated operation.
func (r *EnvironmentReconciler) Reconcile(ctx context.Context, req ctrl.Request) (result ctrl.Result, retErr error) {
	var env divergeiov1alpha1.Environment
	if err := r.Get(ctx, req.NamespacedName, &env); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if tp := env.Annotations["divergedev.com/traceparent"]; tp != "" {
		ctx = otel.GetTextMapPropagator().Extract(ctx, propagation.MapCarrier{"traceparent": tp})
	}
	ctx, span := otel.Tracer("diverge").Start(ctx, "Reconcile")
	defer span.End()
	span.SetAttributes(
		attribute.String("environment.name", env.Name),
		attribute.String("environment.namespace", env.Namespace),
	)

	logger := log.FromContext(ctx)

	startTime := time.Now()

	// Track reconcile outcome — single defer covers all return paths
	defer func() {
		metrics.ReconcileDuration.WithLabelValues("environment").Observe(time.Since(startTime).Seconds())

		if retErr != nil {
			metrics.ReconcileTotal.WithLabelValues("environment", "error").Inc()
			metrics.ReconcileOutcomes.WithLabelValues("error").Inc()
		} else {
			metrics.ReconcileTotal.WithLabelValues("environment", "success").Inc()
			metrics.ReconcileOutcomes.WithLabelValues("success").Inc()
		}

		// Update TTL and cost gauges if applicable.
		// Skip on deletion path — handleTeardown already deleted the series.
		if env.DeletionTimestamp.IsZero() {
			metrics.EstimateEnvironmentCost(&env, time.Now(), metrics.DefaultCostRateCard())
			if env.Status.ExpiresAt != nil {
				remaining := time.Until(env.Status.ExpiresAt.Time).Seconds()
				if remaining < 0 && (env.Status.Phase == divergeiov1alpha1.PhaseTerminating || env.Status.Phase == divergeiov1alpha1.EnvironmentPhase("Expired")) {
					remaining = 0
				}
				metrics.EnvironmentTTLRemaining.WithLabelValues(
					env.Name, env.Namespace,
				).Set(remaining)
			}
		}
	}()

	// Capture a pre-mutation baseline for status patch diffs
	statusBase := env.DeepCopy()

	// 2. Handle deletion
	if !env.DeletionTimestamp.IsZero() {
		return r.handleTeardown(ctx, &env)
	}

	// Copy CommitSHA from spec to status early
	if env.Spec.Source.CommitSHA != "" && env.Status.CommitSHA == "" {
		env.Status.CommitSHA = env.Spec.Source.CommitSHA
	}

	// Initialize metrics from existing state
	for _, route := range env.Spec.Routing.AsyncRoutes {
		asyncActiveRoutes.WithLabelValues(string(route.Protocol)).Add(0)
	}

	// 3. Add finalizer
	if !controllerutil.ContainsFinalizer(&env, environmentFinalizer) {
		controllerutil.AddFinalizer(&env, environmentFinalizer)
		if err := r.Update(ctx, &env); err != nil {
			return ctrl.Result{}, fmt.Errorf("failed to add finalizer: %w", err)
		}
		metrics.EnvironmentsActive.Inc()
		for _, route := range env.Spec.Routing.AsyncRoutes {
			asyncActiveRoutes.WithLabelValues(string(route.Protocol)).Inc()
		}
		if r.Notifier != nil {
			tCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := r.Notifier.PostEnvironmentCreated(tCtx, &env); err != nil {
				logger.Error(err, "failed to post environment created notification")
				r.Recorder.Event(&env, "Warning", "NotificationFailed", err.Error())
			}
		}
		if r.StatusReporter != nil {
			notifyCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
			defer cancel()
			if err := r.StatusReporter.PostCommitStatus(notifyCtx, &env, "pending", "Preview environment provisioning"); err != nil {
				logger.Error(err, "failed to post commit status")
			}
		}
		r.Recorder.Event(&env, "Normal", "Created", "Environment created")
		return ctrl.Result{}, nil
	}

	// 4. Set ObservedGeneration
	env.Status.ObservedGeneration = env.Generation

	// 5-7.6 Provisioning (Namespace, DB, Routing, Async, Banner)
	res, done, err := r.reconcileProvisioning(ctx, &env, statusBase)
	if done {
		return res, err
	}

	// 8. Deploy
	res, done, err = r.reconcileDeploy(ctx, &env, statusBase)
	if done {
		return res, err
	}

	// 10. Derive phase
	newPhase := derivePhase(env.Status.Conditions)
	oldPhase := env.Status.Phase
	env.Status.Phase = newPhase

	if env.Status.Phase == divergeiov1alpha1.PhaseRunning {
		r.Recorder.Event(&env, "Normal", "Running", "Environment is up and running")
	}

	// 11. Lifecycle (TTL, Phase transitions, Testing)
	requeueAfter, res, done, err := r.reconcileLifecycle(ctx, &env, statusBase, oldPhase)
	if done {
		return res, err
	}

	// 12. Update status
	res, err = r.updateStatusWithRequeue(ctx, &env, statusBase, nil, requeueAfter)
	if err == nil && oldPhase != newPhase {
		metrics.EnvironmentTransitions.WithLabelValues(
			string(oldPhase), string(newPhase), env.Spec.Source.Provider,
		).Inc()
	}
	return res, err
}

// SetupWithManager sets up the controller with the Manager.
func (r *EnvironmentReconciler) SetupWithManager(mgr ctrl.Manager) error {
	if r.APIReader == nil && mgr != nil {
		r.APIReader = mgr.GetAPIReader()
	}
	return ctrl.NewControllerManagedBy(mgr).
		For(&divergeiov1alpha1.Environment{}).
		Owns(&batchv1.Job{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Watches(&appsv1.Deployment{}, handler.EnqueueRequestsFromMapFunc(r.mapBaselineDeploymentToEnvironments),
			builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(r.mapConfigMapToEnvironments),
			builder.WithPredicates(predicate.Funcs{
				UpdateFunc: func(e event.UpdateEvent) bool {
					o, ok1 := e.ObjectOld.(*corev1.ConfigMap)
					n, ok2 := e.ObjectNew.(*corev1.ConfigMap)
					return !ok1 || !ok2 || !reflect.DeepEqual(o.Data, n.Data) || !reflect.DeepEqual(o.BinaryData, n.BinaryData)
				},
			})).
		Complete(r)
}

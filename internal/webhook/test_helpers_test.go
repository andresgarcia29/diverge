package webhook

import (
	"fmt"
	"os"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	gatewayv1 "sigs.k8s.io/gateway-api/apis/v1"

	divergeiov1alpha1 "github.com/divergedev/diverge/api/v1alpha1"
)

var testScheme *runtime.Scheme

func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	if err := divergeiov1alpha1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register diverge scheme: %v\n", err)
		os.Exit(1)
	}
	if err := corev1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register corev1 scheme: %v\n", err)
		os.Exit(1)
	}
	if err := appsv1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register appsv1 scheme: %v\n", err)
		os.Exit(1)
	}
	if err := batchv1.AddToScheme(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register batchv1 scheme: %v\n", err)
		os.Exit(1)
	}
	if err := gatewayv1.Install(testScheme); err != nil {
		fmt.Fprintf(os.Stderr, "Failed to register gatewayv1 scheme: %v\n", err)
		os.Exit(1)
	}
	os.Exit(m.Run())
}

func newTestClient(objs ...client.Object) client.WithWatch {
	return fake.NewClientBuilder().WithScheme(testScheme).WithObjects(objs...).Build()
}

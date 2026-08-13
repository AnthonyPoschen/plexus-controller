package controller

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

// TestFactorioRunningStoppedEnvtest exercises reconciliation against a real
// Kubernetes API server. Install envtest assets and set KUBEBUILDER_ASSETS to
// run it; the ordinary unit suite skips it when those local binaries are absent.
func TestFactorioRunningStoppedEnvtest(t *testing.T) {
	if os.Getenv("KUBEBUILDER_ASSETS") == "" {
		t.Skip("set KUBEBUILDER_ASSETS to run the API-server integration scenario")
	}

	environment := &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "kustomization", "base", "crds")},
		ErrorIfCRDPathMissing: true,
	}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Errorf("stop envtest: %v", err)
		}
	})

	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := plexusv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient, err := client.New(config, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if err := kubeClient.Create(ctx, &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "games"}}); err != nil {
		t.Fatal(err)
	}
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.UID = ""
	gameServer.ResourceVersion = ""
	gameServer.Generation = 0
	if err := kubeClient.Create(ctx, testSetupSecret(gameServer)); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Create(ctx, gameServer); err != nil {
		t.Fatal(err)
	}

	reconciler := &GameServerReconciler{Client: kubeClient, Scheme: scheme}
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	var service corev1.Service
	get(t, ctx, kubeClient, request.NamespacedName, &service)
	get(t, ctx, kubeClient, request.NamespacedName, &corev1.PersistentVolumeClaim{})
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := kubeClient.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseRunning {
		t.Fatalf("envtest running phase = %q", current.Status.Phase)
	}
	current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileTwice(t, ctx, reconciler, request)

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStopped {
		t.Fatalf("envtest stopped phase = %q", current.Status.Phase)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &deployment); !apierrors.IsNotFound(err) {
		t.Fatalf("envtest stopped Deployment lookup error = %v, want NotFound", err)
	}
	get(t, ctx, kubeClient, request.NamespacedName, &corev1.PersistentVolumeClaim{})
}

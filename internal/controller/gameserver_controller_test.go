package controller

import (
	"context"
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

func TestFactorioReconcileRunningThenStopped(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	var pvc corev1.PersistentVolumeClaim
	get(t, ctx, kubeClient, request.NamespacedName, &pvc)
	if got := pvc.Spec.Resources.Requests.Storage().String(); got != "50Gi" {
		t.Fatalf("storage request = %q, want 50Gi", got)
	}
	assertOwnedAndLabeled(t, gameServer, &pvc)

	var service corev1.Service
	get(t, ctx, kubeClient, request.NamespacedName, &service)
	assertOwnedAndLabeled(t, gameServer, &service)
	if len(service.Spec.Ports) != 2 || service.Spec.Ports[0].Protocol != corev1.ProtocolUDP {
		t.Fatalf("Factorio service ports = %#v", service.Spec.Ports)
	}
	if service.Spec.Type != corev1.ServiceTypeLoadBalancer {
		t.Fatalf("Factorio service type = %q, want LoadBalancer", service.Spec.Type)
	}

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	assertOwnedAndLabeled(t, gameServer, &deployment)
	if deployment.Spec.Template.Spec.Containers[0].Image != "factoriotools/factorio:stable" {
		t.Fatalf("Factorio image = %q", deployment.Spec.Template.Spec.Containers[0].Image)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath; got != "/factorio" {
		t.Fatalf("persistent mount path = %q", got)
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Labels[plexusv1alpha1.LabelServerID] != "server-1" || current.Labels[plexusv1alpha1.LabelOwnerUserID] != "user-1" || current.Labels[plexusv1alpha1.LabelGameID] != "factorio" {
		t.Fatalf("GameServer labels = %#v", current.Labels)
	}
	if !containsString(current.Finalizers, GameServerFinalizer) {
		t.Fatalf("GameServer finalizers = %#v", current.Finalizers)
	}
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseProvisioning {
		t.Fatalf("initial phase = %q, want Provisioning", current.Status.Phase)
	}
	if current.Status.Endpoint != "" || conditionReason(current, conditionEndpoint) != "LoadBalancerPending" {
		t.Fatalf("initial endpoint status = %#v", current.Status)
	}

	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "factorio.example.com"}}
	if err := kubeClient.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseRunning || current.Status.ActiveSetupID != "setup-1" {
		t.Fatalf("available status = %#v", current.Status)
	}
	if conditionStatus(current, conditionReady) != metav1.ConditionTrue {
		t.Fatalf("Ready condition = %#v", current.Status.Conditions)
	}
	if current.Status.Endpoint != "factorio.example.com:34197" {
		t.Fatalf("running endpoint = %q", current.Status.Endpoint)
	}

	current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
	current.Generation++
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	reconcileOnce(t, ctx, reconciler, request)

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStopped {
		t.Fatalf("stopped phase = %q, want Stopped", current.Status.Phase)
	}
	if current.Status.Endpoint != "" || current.Status.ActiveSetupID != "" {
		t.Fatalf("stopped runtime still reported active: %#v", current.Status)
	}
	if conditionReason(current, conditionReady) != "DesiredStopped" {
		t.Fatalf("stopped Ready condition = %#v", current.Status.Conditions)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stopped Deployment lookup error = %v, want NotFound", err)
	}
	get(t, ctx, kubeClient, request.NamespacedName, &pvc)
	if err := kubeClient.Get(ctx, request.NamespacedName, &service); !apierrors.IsNotFound(err) {
		t.Fatalf("stopped Service lookup error = %v, want NotFound", err)
	}
}

func TestFactorioReconcileReportsFailedForInvalidSchema(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Spec.SelectedSetup.Configuration.SchemaVersion = "factorio/v2"
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed {
		t.Fatalf("phase = %q, want Failed", current.Status.Phase)
	}
	if conditionReason(current, conditionReady) != "InvalidDesiredState" {
		t.Fatalf("Ready condition = %#v", current.Status.Conditions)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid setup Deployment lookup error = %v, want NotFound", err)
	}
}

func TestFactorioReconcileRejectsUnownedRuntimeResource(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	collision := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	reconciler, kubeClient := testReconciler(t, gameServer, collision)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileOnce(t, ctx, reconciler, request)
	if _, err := reconciler.Reconcile(ctx, request); err == nil {
		t.Fatal("expected an ownership collision error")
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed || conditionReason(current, conditionReady) != "StorageReconcileFailed" {
		t.Fatalf("ownership collision status = %#v", current.Status)
	}
}

func TestGameServerDeletionCleansUpOwnedRuntimeResources(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if err := kubeClient.Delete(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	reconcileOnce(t, ctx, reconciler, request)

	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.Service{}, &corev1.PersistentVolumeClaim{}} {
		if err := kubeClient.Get(ctx, request.NamespacedName, object); !apierrors.IsNotFound(err) {
			t.Fatalf("deleted GameServer left %T: %v", object, err)
		}
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &plexusv1alpha1.GameServer{}); !apierrors.IsNotFound(err) {
		t.Fatalf("GameServer finalizer was not released: %v", err)
	}
}

func testGameServer(power plexusv1alpha1.DesiredPower) *plexusv1alpha1.GameServer {
	return &plexusv1alpha1.GameServer{
		ObjectMeta: metav1.ObjectMeta{Name: "factorio-1", Namespace: "games", UID: "gameserver-uid", Generation: 1},
		Spec: plexusv1alpha1.GameServerSpec{
			ServerID: "server-1", OwnerUserID: "user-1", DesiredPower: power,
			ShutdownMode: plexusv1alpha1.ShutdownModeGraceful,
			SelectedSetup: &plexusv1alpha1.SelectedSetupSpec{
				ID: "setup-1", GameID: "factorio",
				Configuration: plexusv1alpha1.GameConfiguration{
					SchemaVersion: "factorio/v1",
					Values:        runtime.RawExtension{Raw: []byte(`{}`)},
					SecretRef:     plexusv1alpha1.SetupSecretReference{Name: "setup-1-secrets"},
				},
			},
		},
	}
}

func testReconciler(t *testing.T, objects ...client.Object) (*GameServerReconciler, client.Client) {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	if err := plexusv1alpha1.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	kubeClient := fake.NewClientBuilder().WithScheme(scheme).
		WithStatusSubresource(&plexusv1alpha1.GameServer{}, &appsv1.Deployment{}, &corev1.Service{}).
		WithObjects(objects...).Build()
	return &GameServerReconciler{Client: kubeClient, Scheme: scheme}, kubeClient
}

func reconcileTwice(t *testing.T, ctx context.Context, reconciler *GameServerReconciler, request ctrl.Request) {
	t.Helper()
	reconcileOnce(t, ctx, reconciler, request)
	reconcileOnce(t, ctx, reconciler, request)
}

func reconcileOnce(t *testing.T, ctx context.Context, reconciler *GameServerReconciler, request ctrl.Request) {
	t.Helper()
	if _, err := reconciler.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
}

func getGameServer(t *testing.T, ctx context.Context, kubeClient client.Client, key client.ObjectKey) *plexusv1alpha1.GameServer {
	t.Helper()
	gameServer := &plexusv1alpha1.GameServer{}
	get(t, ctx, kubeClient, key, gameServer)
	return gameServer
}

func get(t *testing.T, ctx context.Context, kubeClient client.Client, key client.ObjectKey, object client.Object) {
	t.Helper()
	if err := kubeClient.Get(ctx, key, object); err != nil {
		t.Fatal(err)
	}
}

func assertOwnedAndLabeled(t *testing.T, gameServer *plexusv1alpha1.GameServer, object client.Object) {
	t.Helper()
	if object.GetLabels()[plexusv1alpha1.LabelServerID] != gameServer.Spec.ServerID ||
		object.GetLabels()[plexusv1alpha1.LabelOwnerUserID] != gameServer.Spec.OwnerUserID ||
		object.GetLabels()[plexusv1alpha1.LabelGameID] != "factorio" {
		t.Fatalf("%T labels = %#v", object, object.GetLabels())
	}
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.UID != gameServer.UID {
		t.Fatalf("%T controller owner = %#v", object, owner)
	}
}

func conditionStatus(gameServer *plexusv1alpha1.GameServer, conditionType string) metav1.ConditionStatus {
	for _, condition := range gameServer.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Status
		}
	}
	return metav1.ConditionUnknown
}

func conditionReason(gameServer *plexusv1alpha1.GameServer, conditionType string) string {
	for _, condition := range gameServer.Status.Conditions {
		if condition.Type == conditionType {
			return condition.Reason
		}
	}
	return ""
}

func containsString(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

package controller

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/util/validation"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
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
	if deployment.Spec.Template.Spec.Containers[0].Image != "factoriotools/factorio:"+factorio.SupportedRuntimeVersion {
		t.Fatalf("Factorio image = %q", deployment.Spec.Template.Spec.Containers[0].Image)
	}
	if got := deployment.Spec.Template.Spec.Containers[0].VolumeMounts[0].MountPath; got != "/factorio" {
		t.Fatalf("persistent mount path = %q", got)
	}
	container := deployment.Spec.Template.Spec.Containers[0]
	if container.Lifecycle == nil || container.Lifecycle.PreStop == nil || container.Lifecycle.PreStop.Exec == nil ||
		!reflect.DeepEqual(container.Lifecycle.PreStop.Exec.Command, []string{"rcon", "/quit"}) {
		t.Fatalf("Factorio graceful shutdown hook = %#v", container.Lifecycle)
	}
	if deployment.Spec.Template.Spec.TerminationGracePeriodSeconds == nil || *deployment.Spec.Template.Spec.TerminationGracePeriodSeconds != 90 {
		t.Fatalf("Factorio termination grace period = %#v", deployment.Spec.Template.Spec.TerminationGracePeriodSeconds)
	}
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("Factorio rollout strategy = %q, want Recreate to prevent overlapping PVC writers", deployment.Spec.Strategy.Type)
	}

	var configMap corev1.ConfigMap
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g1"}, &configMap)
	assertOwnedAndLabeled(t, gameServer, &configMap)
	if configMap.Immutable == nil || *configMap.Immutable == false {
		t.Fatal("Factorio configuration ConfigMap is mutable")
	}
	wantSettings := `{
  "name": "Plexus Factorio Server",
  "description": "",
  "tags": [],
  "max_players": 0,
  "visibility": {"public": true, "lan": true},
  "require_user_verification": true,
  "allow_commands": "admins-only",
  "autosave_interval": 10,
  "autosave_slots": 5,
  "afk_autokick_interval": 0,
  "auto_pause": true,
  "only_admins_can_pause_the_game": true,
  "autosave_only_on_server": true,
  "non_blocking_saving": false
}`
	assertJSONEqual(t, configMap.Data["server-settings.json"], wantSettings)
	if got := deployment.Spec.Template.Spec.Containers[0].VolumeMounts[1]; got.Name != "factorio-config" || got.MountPath != "/factorio/config" || got.SubPath != "" || got.ReadOnly {
		t.Fatalf("Factorio configuration mount = %#v", got)
	}
	if len(deployment.Spec.Template.Spec.InitContainers) != 2 {
		t.Fatalf("Factorio configuration init containers = %#v", deployment.Spec.Template.Spec.InitContainers)
	}
	configInit := deployment.Spec.Template.Spec.InitContainers[0]
	if configInit.Name != "factorio-config-init" || !reflect.DeepEqual(configInit.Command, []string{"/bin/sh", "-c"}) ||
		!reflect.DeepEqual(configInit.Args, []string{"cp /plexus/config/server-settings.json /factorio/config/server-settings.json"}) {
		t.Fatalf("Factorio configuration init container = %#v", configInit)
	}
	if len(configInit.VolumeMounts) != 2 || configInit.VolumeMounts[0].Name != "factorio-config-source" || !configInit.VolumeMounts[0].ReadOnly || configInit.VolumeMounts[1].Name != "factorio-config" {
		t.Fatalf("Factorio configuration init mounts = %#v", configInit.VolumeMounts)
	}
	modInit := deployment.Spec.Template.Spec.InitContainers[1]
	if modInit.Name != "factorio-mod-sync" || len(modInit.VolumeMounts) != 1 || modInit.VolumeMounts[0].Name != dataVolumeName {
		t.Fatalf("Factorio mod sync init container = %#v", modInit)
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Labels[plexusv1alpha1.LabelServerID] != "server-1" || current.Labels[plexusv1alpha1.LabelOwnerUserID] != "user-1" || current.Labels[plexusv1alpha1.LabelGameID] != "factorio" {
		t.Fatalf("GameServer labels = %#v", current.Labels)
	}
	if !containsString(current.Finalizers, GameServerFinalizer) {
		t.Fatalf("GameServer finalizers = %#v", current.Finalizers)
	}
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStarting {
		t.Fatalf("initial phase = %q, want Starting", current.Status.Phase)
	}
	if current.Spec.DesiredPower != plexusv1alpha1.DesiredPowerRunning {
		t.Fatalf("controller status update overwrote desired power: %q", current.Spec.DesiredPower)
	}
	if current.Status.Endpoint != "" || conditionReason(current, conditionEndpoint) != "LoadBalancerPending" {
		t.Fatalf("initial endpoint status = %#v", current.Status)
	}

	deployment.Status.Replicas = 1
	deployment.Status.AvailableReplicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.ObservedGeneration = deployment.Generation
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
	oldObservation := metav1.NewTime(time.Now().Add(-2 * time.Minute))
	current.Status.LastObservedAt = &oldObservation
	if err := kubeClient.Status().Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if result.RequeueAfter <= 0 || current.Status.LastObservedAt == nil || !current.Status.LastObservedAt.After(oldObservation.Time) {
		t.Fatalf("stable Running observation was not refreshed: result=%#v status=%#v", result, current.Status)
	}

	current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
	current.Generation++
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStopping {
		t.Fatalf("phase while workload deletion is pending = %q, want Stopping", current.Status.Phase)
	}
	if current.Spec.DesiredPower != plexusv1alpha1.DesiredPowerStopped {
		t.Fatalf("controller status update overwrote Stop intent: %q", current.Spec.DesiredPower)
	}
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
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g1"}, &configMap)
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g1-r1"}, &corev1.Secret{})
}

func TestFactorioModArtifactIsInstalledAndReportedOnlyAfterAvailability(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	archive := testFactorioModArchive(t)
	digest := sha256.Sum256(archive)
	sha := hex.EncodeToString(digest[:])
	gameServer.Spec.SelectedSetup.Mods = []plexusv1alpha1.ModSpec{{ProviderID: factorio.ModProviderID, ProviderModID: "tiny-mod", Name: "tiny-mod", Version: "1.2.3", GameVersion: factorio.SupportedFactorioVersion, Dependencies: []string{"base >= 2.0"}, ArchiveFileName: "tiny-mod_1.2.3.zip", ArchiveSHA256: sha, ArtifactRef: "setup-1-mod"}}
	artifact := testModArtifactSecret(gameServer, archive, sha)
	reconciler, kubeClient := testReconciler(t, gameServer, artifact)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	modInit := deployment.Spec.Template.Spec.InitContainers[1]
	if !strings.Contains(modInit.Args[0], "/factorio/mods/tiny-mod_1.2.3.zip") || len(modInit.VolumeMounts) != 2 {
		t.Fatalf("managed mod install init = %#v", modInit)
	}
	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if len(current.Status.InstalledMods) != 0 {
		t.Fatalf("pending workload reported installed mods: %#v", current.Status.InstalledMods)
	}

	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas, deployment.Status.UpdatedReplicas, deployment.Status.AvailableReplicas = 1, 1, 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if len(current.Status.InstalledMods) != 1 || current.Status.InstalledMods[0].Version != "1.2.3" || current.Status.InstalledModsGeneration != current.Generation {
		t.Fatalf("available workload installed mods = %#v", current.Status.InstalledMods)
	}
}

func TestFactorioWorkloadFailuresPreserveFailureTruth(t *testing.T) {
	for _, test := range []struct {
		name, reason string
		withMod      bool
		status       corev1.PodStatus
	}{
		{name: "mod init termination", reason: "ModInstallFailed", withMod: true, status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "factorio-mod-sync", State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 1, Message: "provider-secret-token"}}}}}},
		{name: "image pull", reason: "WorkloadImagePullFailed", status: corev1.PodStatus{InitContainerStatuses: []corev1.ContainerStatus{{Name: "factorio-config-init", State: corev1.ContainerState{Waiting: &corev1.ContainerStateWaiting{Reason: "ImagePullBackOff", Message: "registry-secret"}}}}}},
		{name: "scheduling", reason: "WorkloadSchedulingFailed", status: corev1.PodStatus{Conditions: []corev1.PodCondition{{Type: corev1.PodScheduled, Status: corev1.ConditionFalse, Reason: corev1.PodReasonUnschedulable, Message: "private-node-details"}}}},
		{name: "zero mods ordinary rollout", reason: "WorkloadRolloutFailed"},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
			objects := []client.Object{gameServer}
			if test.withMod {
				archive := testFactorioModArchive(t)
				digest := sha256.Sum256(archive)
				sha := hex.EncodeToString(digest[:])
				gameServer.Spec.SelectedSetup.Mods = []plexusv1alpha1.ModSpec{{ProviderID: factorio.ModProviderID, ProviderModID: "tiny-mod", Name: "tiny-mod", Version: "1.2.3", GameVersion: factorio.SupportedFactorioVersion, Dependencies: []string{"base >= 2.0"}, ArchiveFileName: "tiny-mod_1.2.3.zip", ArchiveSHA256: sha, ArtifactRef: "setup-1-mod"}}
				objects = append(objects, testModArtifactSecret(gameServer, archive, sha))
			}
			reconciler, kubeClient := testReconciler(t, objects...)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
			reconcileTwice(t, ctx, reconciler, request)
			var deployment appsv1.Deployment
			get(t, ctx, kubeClient, request.NamespacedName, &deployment)
			if test.name == "zero mods ordinary rollout" {
				deployment.Status.Conditions = []appsv1.DeploymentCondition{{Type: appsv1.DeploymentReplicaFailure, Status: corev1.ConditionTrue, Reason: "FailedCreate", Message: "provider-secret-token"}}
				if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
					t.Fatal(err)
				}
			} else {
				createOwnedWorkloadPod(t, ctx, kubeClient, gameServer, &deployment, test.status)
			}
			reconcileOnce(t, ctx, reconciler, request)
			current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
			if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed || conditionReason(current, conditionReady) != test.reason {
				t.Fatalf("failure status = %#v", current.Status)
			}
			if strings.Contains(current.Status.Message, "secret") || strings.Contains(current.Status.Message, "private-node") {
				t.Fatalf("failure leaked sensitive detail: %q", current.Status.Message)
			}
		})
	}
}

func TestInstalledModsStayScopedToAvailableConfigurationGeneration(t *testing.T) {
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 2
	gameServer.Spec.SelectedSetup.Mods = []plexusv1alpha1.ModSpec{{ProviderID: factorio.ModProviderID, ProviderModID: "tiny-mod", Name: "tiny-mod", Version: "1.2.4"}}
	previous := plexusv1alpha1.GameServerStatus{ActiveSetupID: "setup-1", ObservedConfigurationGeneration: 1, InstalledModsGeneration: 1, InstalledMods: []plexusv1alpha1.InstalledMod{{ProviderID: factorio.ModProviderID, ProviderModID: "tiny-mod", Name: "tiny-mod", Version: "1.2.3"}}}
	pending := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, "pending")
	preserveActiveRevision(&pending, previous)
	if pending.ObservedGeneration != 2 || pending.InstalledModsGeneration != 1 || pending.InstalledMods[0].Version != "1.2.3" {
		t.Fatalf("pending generation scope = %#v", pending)
	}
	failed := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseFailed, "failed")
	preserveActiveRevision(&failed, pending)
	if failed.InstalledModsGeneration != 1 || failed.InstalledMods[0].Version != "1.2.3" {
		t.Fatalf("failed generation scope = %#v", failed)
	}
	success := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseRunning, "running")
	acknowledgeInstalledMods(&success, gameServer)
	if success.InstalledModsGeneration != 2 || success.InstalledMods[0].Version != "1.2.4" {
		t.Fatalf("success generation scope = %#v", success)
	}
}

func TestFactorioStopUsesCurrentShutdownMode(t *testing.T) {
	for _, test := range []struct {
		name          string
		runningMode   plexusv1alpha1.ShutdownMode
		requestedMode plexusv1alpha1.ShutdownMode
		wantForce     bool
	}{
		{name: "running Force to requested Graceful", runningMode: plexusv1alpha1.ShutdownModeForce, requestedMode: plexusv1alpha1.ShutdownModeGraceful},
		{name: "running Graceful to requested Force", runningMode: plexusv1alpha1.ShutdownModeGraceful, requestedMode: plexusv1alpha1.ShutdownModeForce, wantForce: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx := context.Background()
			gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
			gameServer.Spec.ShutdownMode = test.runningMode
			var podDeleteGracePeriods []*int64
			deploymentMutations := 0
			interceptors := podDeleteInterceptor(&podDeleteGracePeriods)
			interceptors.Create = func(ctx context.Context, kubeClient client.WithWatch, object client.Object, options ...client.CreateOption) error {
				if _, ok := object.(*appsv1.Deployment); ok {
					deploymentMutations++
				}
				return kubeClient.Create(ctx, object, options...)
			}
			interceptors.Update = func(ctx context.Context, kubeClient client.WithWatch, object client.Object, options ...client.UpdateOption) error {
				if _, ok := object.(*appsv1.Deployment); ok {
					deploymentMutations++
				}
				return kubeClient.Update(ctx, object, options...)
			}
			reconciler, kubeClient := testReconcilerWithInterceptors(t, interceptors, gameServer)
			request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
			reconcileTwice(t, ctx, reconciler, request)

			var deployment appsv1.Deployment
			get(t, ctx, kubeClient, request.NamespacedName, &deployment)
			container := deployment.Spec.Template.Spec.Containers[0]
			if container.Lifecycle == nil || container.Lifecycle.PreStop == nil || container.Lifecycle.PreStop.Exec == nil ||
				!reflect.DeepEqual(container.Lifecycle.PreStop.Exec.Command, []string{"rcon", "/quit"}) {
				t.Fatalf("running %s pod graceful shutdown hook = %#v", test.runningMode, container.Lifecycle)
			}
			if grace := deployment.Spec.Template.Spec.TerminationGracePeriodSeconds; grace == nil || *grace != 90 {
				t.Fatalf("running %s pod termination grace period = %#v", test.runningMode, grace)
			}
			pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "factorio-1-pod", Namespace: gameServer.Namespace, Labels: maps.Clone(deployment.Spec.Template.Labels)}}
			if err := kubeClient.Create(ctx, pod); err != nil {
				t.Fatal(err)
			}
			deploymentMutations = 0

			current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
			current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
			current.Spec.ShutdownMode = test.requestedMode
			current.Generation++
			if err := kubeClient.Update(ctx, current); err != nil {
				t.Fatal(err)
			}
			reconcileOnce(t, ctx, reconciler, request)

			if deploymentMutations != 0 {
				t.Fatalf("Stop mutated or recreated the Deployment %d times", deploymentMutations)
			}
			podLookupErr := kubeClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{})
			if test.wantForce {
				if !apierrors.IsNotFound(podLookupErr) {
					t.Fatalf("Force stop Pod lookup error = %v, want NotFound", podLookupErr)
				}
				if len(podDeleteGracePeriods) != 1 || podDeleteGracePeriods[0] == nil || *podDeleteGracePeriods[0] != 0 {
					t.Fatalf("Force stop Pod grace periods = %#v, want [0]", podDeleteGracePeriods)
				}
			} else {
				if podLookupErr != nil {
					t.Fatalf("Graceful stop explicitly removed Pod in fake client: %v", podLookupErr)
				}
				if len(podDeleteGracePeriods) != 0 {
					t.Fatalf("Graceful stop Pod grace periods = %#v, want normal Deployment cascade", podDeleteGracePeriods)
				}
			}
		})
	}
}

func TestFactorioForceEscalatesAnInProgressGracefulStop(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	var podDeleteGracePeriods []*int64
	reconciler, kubeClient := testReconcilerWithInterceptors(t, podDeleteInterceptor(&podDeleteGracePeriods), gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Finalizers = []string{"test.plexus.gg/hold-deletion"}
	if err := kubeClient.Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: "factorio-1-pod", Namespace: gameServer.Namespace, Labels: maps.Clone(deployment.Spec.Template.Labels)}}
	if err := kubeClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
	current.Generation++
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	if deployment.DeletionTimestamp.IsZero() {
		t.Fatal("Graceful stop did not begin Deployment deletion")
	}
	if len(podDeleteGracePeriods) != 0 {
		t.Fatalf("Graceful stop Pod grace periods = %#v, want normal 90-second termination", podDeleteGracePeriods)
	}

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	current.Spec.ShutdownMode = plexusv1alpha1.ShutdownModeForce
	current.Generation++
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(pod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Force escalation Pod lookup error = %v, want NotFound", err)
	}
	if len(podDeleteGracePeriods) != 1 || podDeleteGracePeriods[0] == nil || *podDeleteGracePeriods[0] != 0 {
		t.Fatalf("Force escalation Pod grace periods = %#v, want [0]", podDeleteGracePeriods)
	}
}

func TestForceDeletePodsRequiresGameServerUIDLabel(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerStopped)
	gameServer.Spec.ShutdownMode = plexusv1alpha1.ShutdownModeForce
	ownedPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "owned-pod", Namespace: gameServer.Namespace, Labels: childLabels(gameServer),
	}}
	collidingPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "colliding-pod", Namespace: gameServer.Namespace, Labels: selectorLabels(gameServer),
	}}
	foreignUIDLabels := forceDeletePodLabels(gameServer)
	foreignUIDLabels[plexusv1alpha1.LabelGameServerUID] = "foreign-gameserver-uid"
	foreignUIDPod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "foreign-uid-pod", Namespace: gameServer.Namespace, Labels: foreignUIDLabels,
	}}
	var podDeleteGracePeriods []*int64
	reconciler, kubeClient := testReconcilerWithInterceptors(
		t, podDeleteInterceptor(&podDeleteGracePeriods), gameServer, ownedPod, collidingPod, foreignUIDPod,
	)

	remaining, err := reconciler.forceDeletePods(ctx, gameServer)
	if err != nil {
		t.Fatal(err)
	}
	if !remaining {
		t.Fatal("forceDeletePods reported no UID-bound pods")
	}
	if err := kubeClient.Get(ctx, client.ObjectKeyFromObject(ownedPod), &corev1.Pod{}); !apierrors.IsNotFound(err) {
		t.Fatalf("owned Pod lookup error = %v, want NotFound", err)
	}
	get(t, ctx, kubeClient, client.ObjectKeyFromObject(collidingPod), &corev1.Pod{})
	get(t, ctx, kubeClient, client.ObjectKeyFromObject(foreignUIDPod), &corev1.Pod{})
	if len(podDeleteGracePeriods) != 1 || podDeleteGracePeriods[0] == nil || *podDeleteGracePeriods[0] != 0 {
		t.Fatalf("Pod grace periods = %#v, want [0] for only the UID-bound Pod", podDeleteGracePeriods)
	}
}

func podDeleteInterceptor(gracePeriods *[]*int64) interceptor.Funcs {
	return interceptor.Funcs{
		Delete: func(ctx context.Context, kubeClient client.WithWatch, object client.Object, options ...client.DeleteOption) error {
			if _, ok := object.(*corev1.Pod); ok {
				deleteOptions := &client.DeleteOptions{}
				for _, option := range options {
					option.ApplyToDelete(deleteOptions)
				}
				*gracePeriods = append(*gracePeriods, deleteOptions.GracePeriodSeconds)
			}
			return kubeClient.Delete(ctx, object, options...)
		},
	}
}
func TestFactorioUnloadedServerRetainsRevisionInputsUntilDeletion(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	current.Spec.DesiredPower = plexusv1alpha1.DesiredPowerStopped
	current.Spec.SelectedSetup = nil
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileTwice(t, ctx, reconciler, request)
	reconcileOnce(t, ctx, reconciler, request)

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStopped {
		t.Fatalf("unloaded phase = %q, want Stopped", current.Status.Phase)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("unloaded Deployment lookup error = %v, want NotFound", err)
	}
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g1"}, &corev1.ConfigMap{})
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g1-r1"}, &corev1.Secret{})
}

func TestFactorioReconcileReportsFailedForInvalidSchema(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 4
	gameServer.Status.ObservedGeneration = 3
	gameServer.Spec.SelectedSetup.Configuration.SchemaVersion = "factorio/v2"
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileOnce(t, ctx, reconciler, request)
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != observationRefreshInterval {
		t.Fatalf("invalid schema requeue = %s, want %s", result.RequeueAfter, observationRefreshInterval)
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed {
		t.Fatalf("phase = %q, want Failed", current.Status.Phase)
	}
	if conditionReason(current, conditionReady) != "ConfigurationMigrationRequired" {
		t.Fatalf("Ready condition = %#v", current.Status.Conditions)
	}
	if current.Status.ObservedGeneration != 3 || !strings.Contains(current.Status.Message, "factorio/v1") {
		t.Fatalf("unsupported schema status is not actionable: %#v", current.Status)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("invalid setup Deployment lookup error = %v, want NotFound", err)
	}
}

func TestFactorioRestartWaitsForTheReplacementDeploymentRevision(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}
	reconcileTwice(t, ctx, reconciler, request)

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	var service corev1.Service
	get(t, ctx, kubeClient, request.NamespacedName, &service)
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{Hostname: "factorio.example.test"}}
	if err := kubeClient.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	deployment.Generation = 1
	if err := kubeClient.Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	deployment.Status.ObservedGeneration = 1
	deployment.Status.Replicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	current.Spec.RestartGeneration = 1
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Generation = 2
	deployment.Status.ObservedGeneration = 1
	deployment.Status.Replicas = 2
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	if deployment.Spec.Template.Annotations["plexus.gg/restart-generation"] != "1" {
		t.Fatalf("replacement pod template restart generation = %#v", deployment.Spec.Template.Annotations)
	}
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseStarting || current.Status.ObservedRestartGeneration != 0 {
		t.Fatalf("old available pod acknowledged Restart: %#v", current.Status)
	}

	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseRunning || current.Status.ObservedRestartGeneration != 1 {
		t.Fatalf("replacement deployment was not acknowledged: %#v", current.Status)
	}
}

func TestFactorioReconcileRejectsInvalidStructuredConfiguration(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 5
	gameServer.Status.ObservedGeneration = 4
	gameServer.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{"name":"Copper Works","maxPlayers":70000}`)
	reconciler, kubeClient := testReconciler(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileOnce(t, ctx, reconciler, request)
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	if result.RequeueAfter != observationRefreshInterval {
		t.Fatalf("invalid configuration requeue = %s, want %s", result.RequeueAfter, observationRefreshInterval)
	}

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed || conditionReason(current, conditionReady) != "ConfigurationInvalid" {
		t.Fatalf("invalid configuration status = %#v", current.Status)
	}
	if current.Status.ObservedGeneration != 4 || !strings.Contains(current.Status.Message, "maxPlayers") {
		t.Fatalf("invalid configuration acknowledgement = %#v", current.Status)
	}
	for _, object := range []client.Object{&appsv1.Deployment{}, &corev1.ConfigMap{}, &corev1.Secret{}} {
		if err := kubeClient.Get(ctx, request.NamespacedName, object); !apierrors.IsNotFound(err) {
			t.Fatalf("invalid configuration created %T: %v", object, err)
		}
	}
}

func TestFactorioReconcileReportsSecretMigrationFailureWithoutDisclosure(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 6
	gameServer.Status.ObservedGeneration = 5
	secret := testSetupSecret(t, gameServer)
	secret.Annotations[factorio.SecretSchemaAnnotation] = "factorio-secrets/v0"
	secret.Data[factorio.SecretDataKey] = marshalTestJSON(t, map[string]string{"legacyPassword": testSecretValue("legacy")})
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, secret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedGeneration != 5 || conditionReason(current, conditionReady) != "SetupSecretMigrationRequired" {
		t.Fatalf("Secret migration status = %#v", current.Status)
	}
	if !strings.Contains(current.Status.Message, factorio.SecretSchemaVersion) || strings.Contains(current.Status.Message, "must-not-appear") {
		t.Fatalf("Secret migration status is unsafe or not actionable: %q", current.Status.Message)
	}
	if err := kubeClient.Get(ctx, request.NamespacedName, &appsv1.Deployment{}); !apierrors.IsNotFound(err) {
		t.Fatalf("Secret migration failure created a workload: %v", err)
	}
}

func TestFactorioRunningStatusAcknowledgesActiveConfigurationAndSecretRevision(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 9
	gameServer.Spec.RestartGeneration = 5
	gameServer.Status = plexusv1alpha1.GameServerStatus{
		Phase: plexusv1alpha1.GameServerPhaseRunning, ActiveSetupID: "setup-1", ObservedGeneration: 8,
		ObservedRestartGeneration: 4, ObservedConfigurationGeneration: 8, ObservedSecretRevision: 3,
	}
	secret := testSetupSecret(t, gameServer)
	secret.Annotations[factorio.SecretRevisionAnnotation] = "4"
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, secret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)
	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	if deployment.Spec.Template.Annotations["plexus.gg/configuration-generation"] != "9" || deployment.Spec.Template.Annotations[factorio.SecretRevisionAnnotation] != "4" {
		t.Fatalf("workload revision annotations = %#v", deployment.Spec.Template.Annotations)
	}
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	var service corev1.Service
	get(t, ctx, kubeClient, request.NamespacedName, &service)
	result, err := reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	pending := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	statusJSON := currentStatusText(pending)
	var pendingStatus map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &pendingStatus); err != nil {
		t.Fatal(err)
	}
	if pendingStatus["observedConfigurationGeneration"] != float64(8) || pendingStatus["observedSecretRevision"] != float64(3) {
		t.Fatalf("rollout did not preserve the previously active revision: %s", statusJSON)
	}
	if pending.Status.Phase != plexusv1alpha1.GameServerPhaseStarting || pending.Status.ObservedGeneration != 9 ||
		pending.Status.ObservedRestartGeneration != 4 || pending.Status.ActiveSetupID != "setup-1" || result.RequeueAfter != observationRefreshInterval {
		t.Fatalf("pending rollout lifecycle status = %#v, result=%#v", pending.Status, result)
	}

	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	endpointPending := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	statusJSON = currentStatusText(endpointPending)
	var endpointPendingStatus map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &endpointPendingStatus); err != nil {
		t.Fatal(err)
	}
	if endpointPendingStatus["observedConfigurationGeneration"] != float64(9) || endpointPendingStatus["observedSecretRevision"] != float64(4) {
		t.Fatalf("available workload did not acknowledge active revisions while its endpoint was pending: %s", statusJSON)
	}
	if endpointPending.Status.Phase != plexusv1alpha1.GameServerPhaseStarting || endpointPending.Status.ObservedRestartGeneration != 5 ||
		endpointPending.Status.ActiveSetupID != "setup-1" || result.RequeueAfter != observationRefreshInterval {
		t.Fatalf("endpoint-pending lifecycle status = %#v, result=%#v", endpointPending.Status, result)
	}

	get(t, ctx, kubeClient, request.NamespacedName, &service)
	service.Status.LoadBalancer.Ingress = []corev1.LoadBalancerIngress{{IP: "192.0.2.10"}}
	if err := kubeClient.Status().Update(ctx, &service); err != nil {
		t.Fatal(err)
	}
	result, err = reconciler.Reconcile(ctx, request)
	if err != nil {
		t.Fatal(err)
	}

	running := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	statusJSON = currentStatusText(running)
	var status map[string]any
	if err := json.Unmarshal([]byte(statusJSON), &status); err != nil {
		t.Fatal(err)
	}
	if status["observedConfigurationGeneration"] != float64(9) || status["observedSecretRevision"] != float64(4) {
		t.Fatalf("active revision acknowledgement = %s", statusJSON)
	}
	if running.Status.Phase != plexusv1alpha1.GameServerPhaseRunning || running.Status.ObservedRestartGeneration != 5 ||
		result.RequeueAfter != observationRefreshInterval {
		t.Fatalf("running lifecycle status = %#v, result=%#v", running.Status, result)
	}
}

func TestFactorioRolloutDoesNotAcknowledgeUntilOnlyUpdatedReplicaIsAvailable(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 9
	gameServer.Status = plexusv1alpha1.GameServerStatus{
		Phase: plexusv1alpha1.GameServerPhaseRunning, ActiveSetupID: "setup-1", ObservedGeneration: 8,
		ObservedConfigurationGeneration: 8, ObservedSecretRevision: 3,
	}
	secret := testSetupSecret(t, gameServer)
	secret.Annotations[factorio.SecretRevisionAnnotation] = "4"
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, secret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)
	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = 2
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedConfigurationGeneration != 8 || current.Status.ObservedSecretRevision != 3 {
		t.Fatalf("rollout with an extra old replica acknowledged replacement inputs: %#v", current.Status)
	}

	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Status.Replicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedConfigurationGeneration != 9 || current.Status.ObservedSecretRevision != 4 {
		t.Fatalf("completed rollout did not acknowledge replacement inputs: %#v", current.Status)
	}
}

func TestFactorioRolloutKeepsPreviousPodTemplatePinnedToImmutableInputs(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 8
	gameServer.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{}`)
	oldSetupSecret := testSetupSecret(t, gameServer)
	oldSetupSecret.Annotations[factorio.SecretRevisionAnnotation] = "3"
	oldSecretValues := factorio.Secrets{
		Username: testSecretValue("old-account"), Token: testSecretValue("old-token"), RCONPassword: testSecretValue("old-rcon"),
	}
	oldSetupSecret.Data[factorio.SecretDataKey] = marshalTestJSON(t, oldSecretValues)
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, oldSetupSecret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)
	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	previousTemplate := deployment.Spec.Template.DeepCopy()
	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	replacement := testSetupSecret(t, current)
	replacement.Name = "setup-1-secrets-r4"
	replacement.Annotations[factorio.SecretRevisionAnnotation] = "4"
	replacement.Data[factorio.SecretDataKey] = marshalTestJSON(t, factorio.Secrets{
		Username: testSecretValue("new-account"), Token: testSecretValue("new-token"), RCONPassword: testSecretValue("new-rcon"),
	})
	if err := kubeClient.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RollingUpdateDeploymentStrategyType}
	if err := kubeClient.Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	deployment.Status.ObservedGeneration = 0
	deployment.Status.UpdatedReplicas = 0
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	current.Spec.SelectedSetup.Configuration.SecretRef.Name = replacement.Name
	current.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{
		"name":"New factory","maxPlayers":20,"visibility":{"public":false,"lan":true},
		"allowCommands":"admins-only","autosave":{"intervalMinutes":10,"slots":5}
	}`)
	current.Generation = 9
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	assertPodTemplateRuntimeInputs(t, previousTemplate, "factorio-1-config-g8", "factorio-1-runtime-g8-r3")
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	if deployment.Spec.Strategy.Type != appsv1.RecreateDeploymentStrategyType {
		t.Fatalf("configuration rollout strategy = %q, want Recreate to prevent overlapping PVC writers", deployment.Spec.Strategy.Type)
	}
	assertPodTemplateRuntimeInputs(t, &deployment.Spec.Template, "factorio-1-config-g9", "factorio-1-runtime-g9-r4")

	var oldConfig corev1.ConfigMap
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g8"}, &oldConfig)
	if oldConfig.Immutable == nil || *oldConfig.Immutable == false || !strings.Contains(oldConfig.Data[configFileName], "Plexus Factorio Server") {
		t.Fatalf("previous ConfigMap changed during rollout: %#v", oldConfig)
	}
	var oldRuntimeSecret corev1.Secret
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g8-r3"}, &oldRuntimeSecret)
	if oldRuntimeSecret.Immutable == nil || *oldRuntimeSecret.Immutable == false || string(oldRuntimeSecret.Data["TOKEN"]) != oldSecretValues.Token {
		t.Fatal("previous runtime Secret changed during rollout")
	}
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedConfigurationGeneration != 8 || current.Status.ObservedSecretRevision != 3 {
		t.Fatalf("pending rollout acknowledged replacement inputs: %#v", current.Status)
	}

	deployment.Status.ObservedGeneration = deployment.Generation
	deployment.Status.Replicas = 1
	deployment.Status.UpdatedReplicas = 1
	deployment.Status.AvailableReplicas = 1
	if err := kubeClient.Status().Update(ctx, &deployment); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedConfigurationGeneration != 9 || current.Status.ObservedSecretRevision != 4 {
		t.Fatalf("available rollout did not acknowledge replacement inputs: %#v", current.Status)
	}
}

func TestRevisionScopedRuntimeNamesAreDeterministicDNSLabels(t *testing.T) {
	gameServerName := strings.Repeat("factorio.", 30) + "server"
	for _, suffix := range []string{
		"-config-g9223372036854775807",
		"-runtime-g9223372036854775807-r9223372036854775807",
	} {
		got := revisionScopedResourceName(gameServerName, suffix)
		if errors := validation.IsDNS1123Label(got); len(errors) != 0 {
			t.Fatalf("revision-scoped name %q is not a DNS label: %v", got, errors)
		}
		if repeated := revisionScopedResourceName(gameServerName, suffix); repeated != got {
			t.Fatalf("revision-scoped name changed from %q to %q", got, repeated)
		}
	}
}

func TestFactorioStoppedEditIsAcknowledgedAndNextStartRendersExactEnvelope(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerStopped)
	gameServer.Generation = 1
	gameServer.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{
		"name":"Old Factory","description":"Must be cleared","tags":["old"],"maxPlayers":16
	}`)
	gameServer.Spec.SelectedSetup.Configuration.SecretRef.Name = "setup-1-secrets-r1"
	oldSecret := testSetupSecret(t, gameServer)
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, oldSecret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)
	stopped := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if stopped.Status.Phase != plexusv1alpha1.GameServerPhaseStopped || stopped.Status.ObservedGeneration != 1 {
		t.Fatalf("initial stopped configuration was not acknowledged: %#v", stopped.Status)
	}

	gameServer.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{
		"name":"Acceptance Factory","maxPlayers":32,"autosave":{"intervalMinutes":7,"slots":6}
	}`)
	gameServer.Spec.SelectedSetup.Configuration.SecretRef.Name = "setup-1-secrets-r2"
	gameServer.Generation = 2
	replacementSecret := testSetupSecret(t, gameServer)
	replacementSecret.Annotations[factorio.SecretRevisionAnnotation] = "2"
	replacementSecretValues := factorio.Secrets{
		Username:     testSecretValue("replacement-account"),
		Token:        testSecretValue("replacement-token"),
		GamePassword: testSecretValue("replacement-join"),
		RCONPassword: testSecretValue("replacement-rcon"),
	}
	replacementSecret.Data[factorio.SecretDataKey] = marshalTestJSON(t, replacementSecretValues)
	if err := kubeClient.Create(ctx, replacementSecret); err != nil {
		t.Fatal(err)
	}
	stopped.Spec.SelectedSetup.Configuration = gameServer.Spec.SelectedSetup.Configuration
	stopped.Generation = 2
	if err := kubeClient.Update(ctx, stopped); err != nil {
		t.Fatal(err)
	}

	reconcileTwice(t, ctx, reconciler, request)
	stopped = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if stopped.Status.Phase != plexusv1alpha1.GameServerPhaseStopped || stopped.Status.ObservedGeneration != 2 {
		t.Fatalf("stopped edit was not acknowledged: %#v", stopped.Status)
	}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g2"}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stopped edit rendered runtime input before Start: %v", err)
	}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g2-r2"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("stopped edit rendered runtime Secret before Start: %v", err)
	}

	stopped.Spec.DesiredPower = plexusv1alpha1.DesiredPowerRunning
	stopped.Generation = 3
	if err := kubeClient.Update(ctx, stopped); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)

	var configMap corev1.ConfigMap
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g3"}, &configMap)
	assertJSONEqual(t, configMap.Data[configFileName], `{
		"name":"Acceptance Factory","max_players":32,"autosave_interval":7,"autosave_slots":6,
		"description":"","tags":[],"visibility":{"public":true,"lan":true},
		"require_user_verification":true,"allow_commands":"admins-only","afk_autokick_interval":0,
		"auto_pause":true,"only_admins_can_pause_the_game":true,"autosave_only_on_server":true,
		"non_blocking_saving":false
	}`)
	var runtimeSecret corev1.Secret
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g3-r2"}, &runtimeSecret)
	wantRuntimeSecretData := map[string][]byte{
		"USERNAME": []byte(replacementSecretValues.Username), "TOKEN": []byte(replacementSecretValues.Token),
		"GAME_PASSWORD": []byte(replacementSecretValues.GamePassword), "RCON_PASSWORD": []byte(replacementSecretValues.RCONPassword),
	}
	if !reflect.DeepEqual(runtimeSecret.Data, wantRuntimeSecretData) {
		t.Fatalf("runtime Secret data did not use the replacement setup Secret: %#v", runtimeSecret.Data)
	}
	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	assertPodTemplateRuntimeInputs(t, &deployment.Spec.Template, "factorio-1-config-g3", "factorio-1-runtime-g3-r2")
}

func TestFactorioReconcileRendersCustomConfigurationAndSecretBackedEnvironment(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Spec.SelectedSetup.Configuration.Values.Raw = []byte(`{
		"name":"Copper Works","description":"Private factory","tags":["co-op","vanilla"],"maxPlayers":20,
		"visibility":{"public":false,"lan":true},"requireUserVerification":false,"allowCommands":"true",
		"autosave":{"intervalMinutes":3,"slots":12},"afkAutokickMinutes":45,"autoPause":false,
		"onlyAdminsCanPause":false,"autosaveOnlyOnServer":false,"nonBlockingSaving":true
	}`)
	secretValues := factorio.Secrets{
		Username:     testSecretValue("account"),
		Token:        testSecretValue("portal-token"),
		GamePassword: testSecretValue("join"),
		RCONPassword: testSecretValue("rcon"),
	}
	secret := testSetupSecret(t, gameServer)
	secret.Data[factorio.SecretDataKey] = marshalTestJSON(t, secretValues)
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer, secret)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	var configMap corev1.ConfigMap
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g1"}, &configMap)
	wantSettings := `{
		"name":"Copper Works","description":"Private factory","tags":["co-op","vanilla"],"max_players":20,
		"visibility":{"public":false,"lan":true},"require_user_verification":false,"allow_commands":"true",
		"autosave_interval":3,"autosave_slots":12,"afk_autokick_interval":45,"auto_pause":false,
		"only_admins_can_pause_the_game":false,"autosave_only_on_server":false,"non_blocking_saving":true
	}`
	assertJSONEqual(t, configMap.Data[configFileName], wantSettings)

	var runtimeSecret corev1.Secret
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g1-r1"}, &runtimeSecret)
	assertOwnedAndLabeled(t, gameServer, &runtimeSecret)
	if runtimeSecret.Immutable == nil || *runtimeSecret.Immutable == false {
		t.Fatal("Factorio runtime Secret is mutable")
	}
	wantSecretData := map[string][]byte{
		"USERNAME": []byte(secretValues.Username), "TOKEN": []byte(secretValues.Token),
		"GAME_PASSWORD": []byte(secretValues.GamePassword), "RCON_PASSWORD": []byte(secretValues.RCONPassword),
	}
	if !reflect.DeepEqual(runtimeSecret.Data, wantSecretData) {
		t.Fatal("runtime Secret did not contain the expected protected values")
	}

	var deployment appsv1.Deployment
	get(t, ctx, kubeClient, request.NamespacedName, &deployment)
	container := deployment.Spec.Template.Spec.Containers[0]
	for _, name := range []string{"USERNAME", "TOKEN", "GAME_PASSWORD", "RCON_PASSWORD"} {
		environment := findEnvironment(container.Env, name)
		if environment == nil || environment.Value != "" || environment.ValueFrom == nil || environment.ValueFrom.SecretKeyRef == nil ||
			environment.ValueFrom.SecretKeyRef.Name != "factorio-1-runtime-g1-r1" || environment.ValueFrom.SecretKeyRef.Key != name {
			t.Fatalf("%s environment = %#v", name, environment)
		}
	}
	deploymentJSON, err := json.Marshal(deployment)
	if err != nil {
		t.Fatal(err)
	}
	exposed := configMap.Data[configFileName] + string(deploymentJSON) + currentStatusText(getGameServer(t, ctx, kubeClient, request.NamespacedName))
	for _, sensitive := range []string{secretValues.Username, secretValues.Token, secretValues.GamePassword, secretValues.RCONPassword} {
		if strings.Contains(exposed, sensitive) {
			t.Fatal("a sensitive fixture was exposed outside a Secret")
		}
	}
}

func TestFactorioSecretMustValidateBeforeGenerationIsObserved(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerStopped)
	gameServer.Generation = 7
	gameServer.Status.ObservedGeneration = 6
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)
	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedGeneration != 6 {
		t.Fatalf("missing Secret generation was acknowledged: %#v", current.Status)
	}
	if conditionReason(current, conditionReady) != "SetupSecretInvalid" {
		t.Fatalf("missing Secret condition = %#v", current.Status.Conditions)
	}

	secret := testSetupSecret(t, gameServer)
	invalidSecretMaterial := testSecretValue("unpaired-token")
	secret.Data[factorio.SecretDataKey] = marshalTestJSON(t, factorio.Secrets{
		Token: invalidSecretMaterial, RCONPassword: testSecretValue("rcon"),
	})
	if err := kubeClient.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if strings.Contains(current.Status.Message, invalidSecretMaterial) {
		t.Fatalf("invalid Secret material reached status: %q", current.Status.Message)
	}
	if current.Status.ObservedGeneration != 6 {
		t.Fatalf("invalid Secret generation was acknowledged: %#v", current.Status)
	}

	replacement := testSetupSecret(t, gameServer)
	replacement.Name = "setup-1-secrets-r2"
	replacement.Annotations[factorio.SecretRevisionAnnotation] = "2"
	if err := kubeClient.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	current.Spec.SelectedSetup.Configuration.SecretRef.Name = replacement.Name
	current.Generation = 8
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.ObservedGeneration != 8 || current.Status.Phase != plexusv1alpha1.GameServerPhaseStopped {
		t.Fatalf("valid Secret was not acknowledged: %#v", current.Status)
	}
}

func TestFactorioInvalidReplacementSecretPreservesLiveRuntimeStatus(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Generation = 8
	players := int32(4)
	gameServer.Status = plexusv1alpha1.GameServerStatus{
		Phase:                           plexusv1alpha1.GameServerPhaseRunning,
		ActiveSetupID:                   "setup-1",
		ObservedGeneration:              7,
		ObservedRestartGeneration:       3,
		ObservedConfigurationGeneration: 7,
		ObservedSecretRevision:          1,
		Endpoint:                        "factorio.example.test:34197",
		Players:                         &players,
	}
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseRunning ||
		current.Status.ActiveSetupID != "setup-1" || current.Status.Endpoint != "factorio.example.test:34197" ||
		current.Status.Players == nil || *current.Status.Players != 4 {
		t.Fatalf("invalid replacement erased live runtime status: %#v", current.Status)
	}
	if current.Status.ObservedGeneration != 7 || current.Status.ObservedRestartGeneration != 3 {
		t.Fatalf("invalid replacement was observed: %#v", current.Status)
	}
	if current.Status.ObservedConfigurationGeneration != 7 || current.Status.ObservedSecretRevision != 1 {
		t.Fatalf("invalid replacement erased the active revisions: %#v", current.Status)
	}
	if conditionReason(current, conditionReady) != "SetupSecretInvalid" {
		t.Fatalf("invalid replacement condition = %#v", current.Status.Conditions)
	}
}

func TestUnsupportedGameDoesNotRequireFactorioSecret(t *testing.T) {
	ctx := context.Background()
	gameServer := testGameServer(plexusv1alpha1.DesiredPowerRunning)
	gameServer.Spec.SelectedSetup.GameID = "project-zomboid"
	gameServer.Spec.SelectedSetup.Configuration.SchemaVersion = "project-zomboid/v1"
	reconciler, kubeClient := testReconcilerWithoutSecret(t, gameServer)
	request := ctrl.Request{NamespacedName: client.ObjectKeyFromObject(gameServer)}

	reconcileTwice(t, ctx, reconciler, request)

	current := getGameServer(t, ctx, kubeClient, request.NamespacedName)
	if current.Status.Phase != plexusv1alpha1.GameServerPhaseFailed || conditionReason(current, conditionReady) != "UnsupportedGame" {
		t.Fatalf("unsupported game status = %#v", current.Status)
	}
	if strings.Contains(current.Status.Message, "Secret") {
		t.Fatalf("unsupported game incorrectly required a Factorio Secret: %q", current.Status.Message)
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
	replacement := testSetupSecret(t, current)
	replacement.Name = "setup-1-secrets-r2"
	replacement.Annotations[factorio.SecretRevisionAnnotation] = "2"
	if err := kubeClient.Create(ctx, replacement); err != nil {
		t.Fatal(err)
	}
	current.Spec.SelectedSetup.Configuration.SecretRef.Name = replacement.Name
	current.Generation = 2
	if err := kubeClient.Update(ctx, current); err != nil {
		t.Fatal(err)
	}
	reconcileOnce(t, ctx, reconciler, request)
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-config-g2"}, &corev1.ConfigMap{})
	get(t, ctx, kubeClient, client.ObjectKey{Namespace: gameServer.Namespace, Name: "factorio-1-runtime-g2-r2"}, &corev1.Secret{})
	current = getGameServer(t, ctx, kubeClient, request.NamespacedName)
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
	for _, name := range []string{"factorio-1-config-g1", "factorio-1-config-g2"} {
		if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: name}, &corev1.ConfigMap{}); !apierrors.IsNotFound(err) {
			t.Fatalf("deleted GameServer left ConfigMap %q: %v", name, err)
		}
	}
	for _, name := range []string{"factorio-1-runtime-g1-r1", "factorio-1-runtime-g2-r2"} {
		if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: name}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
			t.Fatalf("deleted GameServer left runtime Secret %q: %v", name, err)
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
	return testReconcilerWithInterceptors(t, interceptor.Funcs{}, objects...)
}

func testReconcilerWithInterceptors(t *testing.T, interceptors interceptor.Funcs, objects ...client.Object) (*GameServerReconciler, client.Client) {
	for _, object := range append([]client.Object(nil), objects...) {
		if gameServer, ok := object.(*plexusv1alpha1.GameServer); ok && gameServer.Spec.SelectedSetup != nil {
			objects = append(objects, testSetupSecret(t, gameServer))
		}
	}
	return testReconcilerWithoutSecretWithInterceptors(t, interceptors, objects...)
}

func testReconcilerWithoutSecret(t *testing.T, objects ...client.Object) (*GameServerReconciler, client.Client) {
	return testReconcilerWithoutSecretWithInterceptors(t, interceptor.Funcs{}, objects...)
}

func testReconcilerWithoutSecretWithInterceptors(t *testing.T, interceptors interceptor.Funcs, objects ...client.Object) (*GameServerReconciler, client.Client) {
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
		WithObjects(objects...).WithInterceptorFuncs(interceptors).Build()
	return &GameServerReconciler{Client: kubeClient, Scheme: scheme}, kubeClient
}

func createOwnedWorkloadPod(t *testing.T, ctx context.Context, kubeClient client.Client, gameServer *plexusv1alpha1.GameServer, deployment *appsv1.Deployment, status corev1.PodStatus) {
	t.Helper()
	controlled := true
	replicaSet := &appsv1.ReplicaSet{ObjectMeta: metav1.ObjectMeta{Name: deployment.Name + "-rs", Namespace: deployment.Namespace, UID: "replicaset-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "Deployment", Name: deployment.Name, UID: deployment.UID, Controller: &controlled}}}}
	if err := kubeClient.Create(ctx, replicaSet); err != nil {
		t.Fatal(err)
	}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: deployment.Name + "-pod", Namespace: deployment.Namespace, Labels: selectorLabels(gameServer), OwnerReferences: []metav1.OwnerReference{{APIVersion: "apps/v1", Kind: "ReplicaSet", Name: replicaSet.Name, UID: replicaSet.UID, Controller: &controlled}}}, Status: status}
	pod.Annotations = map[string]string{"plexus.gg/configuration-generation": fmt.Sprint(gameServer.Generation)}
	if err := kubeClient.Create(ctx, pod); err != nil {
		t.Fatal(err)
	}
}

func testSetupSecret(t *testing.T, gameServer *plexusv1alpha1.GameServer) *corev1.Secret {
	t.Helper()
	immutable := true
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: gameServer.Spec.SelectedSetup.Configuration.SecretRef.Name, Namespace: gameServer.Namespace,
			Labels: map[string]string{
				plexusv1alpha1.LabelServerID: gameServer.Spec.ServerID, plexusv1alpha1.LabelOwnerUserID: gameServer.Spec.OwnerUserID,
				plexusv1alpha1.LabelGameID: gameServer.Spec.SelectedSetup.GameID, plexusv1alpha1.LabelSetupID: gameServer.Spec.SelectedSetup.ID,
			},
			Annotations: map[string]string{factorio.SecretSchemaAnnotation: factorio.SecretSchemaVersion, factorio.SecretRevisionAnnotation: "1"},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data: map[string][]byte{factorio.SecretDataKey: marshalTestJSON(t, factorio.Secrets{
			RCONPassword: testSecretValue("rcon"),
		})},
	}
}

func testFactorioModArchive(t *testing.T) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	entry, err := writer.Create("tiny-mod_1.2.3/info.json")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := entry.Write([]byte(`{"name":"tiny-mod","version":"1.2.3","factorio_version":"2.0","dependencies":["base >= 2.0"]}`)); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func testModArtifactSecret(gameServer *plexusv1alpha1.GameServer, archive []byte, sha string) *corev1.Secret {
	immutable := true
	controller := true
	blockOwnerDeletion := true
	return &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "setup-1-mod", Namespace: gameServer.Namespace,
		Labels:          map[string]string{plexusv1alpha1.LabelServerID: gameServer.Spec.ServerID, plexusv1alpha1.LabelOwnerUserID: gameServer.Spec.OwnerUserID, plexusv1alpha1.LabelGameID: factorio.GameID, plexusv1alpha1.LabelSetupID: gameServer.Spec.SelectedSetup.ID},
		Annotations:     map[string]string{factorio.ModProviderAnnotation: factorio.ModProviderID, factorio.ModIDAnnotation: "tiny-mod", factorio.ModVersionAnnotation: "1.2.3", factorio.ModSHA256Annotation: sha},
		OwnerReferences: []metav1.OwnerReference{{APIVersion: plexusv1alpha1.GroupVersion.String(), Kind: "GameServer", Name: gameServer.Name, UID: gameServer.UID, Controller: &controller, BlockOwnerDeletion: &blockOwnerDeletion}}},
		Immutable: &immutable, Type: corev1.SecretTypeOpaque, Data: map[string][]byte{factorio.ModArtifactDataKey: archive}}
}

func marshalTestJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func testSecretValue(label string) string {
	return label + "-" + strings.Repeat("x", 24)
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
		object.GetLabels()[plexusv1alpha1.LabelGameID] != "factorio" || object.GetLabels()[plexusv1alpha1.LabelSetupID] != "setup-1" {
		t.Fatalf("%T labels = %#v", object, object.GetLabels())
	}
	owner := metav1.GetControllerOf(object)
	if owner == nil || owner.UID != gameServer.UID {
		t.Fatalf("%T controller owner = %#v", object, owner)
	}
}

func assertJSONEqual(t *testing.T, got string, want string) {
	t.Helper()
	var gotValue any
	if err := json.Unmarshal([]byte(got), &gotValue); err != nil {
		t.Fatalf("rendered JSON is invalid: %v\n%s", err, got)
	}
	var wantValue any
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("rendered JSON = %s, want %s", got, want)
	}
}

func assertPodTemplateRuntimeInputs(t *testing.T, template *corev1.PodTemplateSpec, wantConfigMap string, wantSecret string) {
	t.Helper()
	if got := template.Spec.Volumes[1].ConfigMap.Name; got != wantConfigMap {
		t.Fatalf("pod template ConfigMap = %q, want %q", got, wantConfigMap)
	}
	for _, name := range []string{"USERNAME", "TOKEN", "GAME_PASSWORD", "RCON_PASSWORD"} {
		environment := findEnvironment(template.Spec.Containers[0].Env, name)
		if environment == nil || environment.ValueFrom == nil || environment.ValueFrom.SecretKeyRef == nil || environment.ValueFrom.SecretKeyRef.Name != wantSecret {
			t.Fatalf("pod template %s Secret reference = %#v, want %q", name, environment, wantSecret)
		}
	}
}

func findEnvironment(environment []corev1.EnvVar, name string) *corev1.EnvVar {
	for index := range environment {
		if environment[index].Name == name {
			return &environment[index]
		}
	}
	return nil
}

func currentStatusText(gameServer *plexusv1alpha1.GameServer) string {
	data, _ := json.Marshal(gameServer.Status)
	return string(data)
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

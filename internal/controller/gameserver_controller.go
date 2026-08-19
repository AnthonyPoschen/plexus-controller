package controller

//go:generate go run sigs.k8s.io/controller-tools/cmd/controller-gen rbac:roleName=plexus-controller paths=./... output:rbac:artifacts:config=../../kustomization/base

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"sort"
	"strconv"
	"strings"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/internal/games"
	"github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement"
	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
	zomboid "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/projectzomboid/v1"
)

const (
	// GameServerFinalizer keeps the GameServer present until its controller-owned
	// runtime resources have been removed.
	GameServerFinalizer = "plexus.gg/runtime-cleanup"

	conditionReady             = "Ready"
	conditionStorage           = "StorageReady"
	conditionEndpoint          = "EndpointReady"
	conditionMods              = "ModsReady"
	conditionDiskJob           = "DiskJob"
	conditionShutdown          = "ShutdownProgress"
	takingLongerThanExpected   = "Taking longer than expected"
	dataVolumeName             = "game-data"
	modSourceName              = "factorio-mod-source"
	modSourcePath              = "/plexus/mod"
	observationRefreshInterval = 30 * time.Second
)

// GameServerReconciler turns GameServers into their persistent storage,
// sticky network Service, and desired running or stopped workload.
type GameServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	Now    func() time.Time
}

// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=list;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=plexus.gg,resources=saveimports;saveexports,verbs=get;list;watch

func (r *GameServerReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var gameServer plexusv1alpha1.GameServer
	if err := r.Get(ctx, request.NamespacedName, &gameServer); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !gameServer.DeletionTimestamp.IsZero() {
		return r.reconcileDelete(ctx, &gameServer)
	}

	if changed, err := r.ensureMetadata(ctx, &gameServer); err != nil {
		return ctrl.Result{}, err
	} else if changed {
		return ctrl.Result{Requeue: true}, nil
	}

	if err := validateDesiredState(&gameServer); err != nil {
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportPermanentFailure(ctx, &gameServer, "InvalidDesiredState", err)
	}

	if gameServer.Spec.SelectedSetup == nil {
		return r.reconcileUnloaded(ctx, &gameServer)
	}
	definition, err := games.Get(gameServer.Spec.SelectedSetup.GameID)
	if err != nil || definition.Workload.ContainerName == "" {
		if err == nil {
			err = fmt.Errorf("game %q does not have a workload reconciler", definition.ID)
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportPermanentFailure(ctx, &gameServer, "UnsupportedGame", err)
	}
	if gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerStopped {
		handled, result, err := r.quiesceBeforeSecretValidation(ctx, &gameServer, definition)
		if err != nil || handled {
			return result, err
		}
	}
	if gameServer.Spec.SelectedSetup.Configuration.SchemaVersion != definition.ManagementSchemaVersion {
		err := fmt.Errorf("%s setup schemaVersion is %q; migrate the setup to supported schema %q", definition.DisplayName, gameServer.Spec.SelectedSetup.Configuration.SchemaVersion, definition.ManagementSchemaVersion)
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportUnobservedFailure(ctx, &gameServer, "ConfigurationMigrationRequired", err)
	}
	secretEnv, secretRevision, err := r.validateSetupSecret(ctx, &gameServer, definition)
	if err != nil {
		reason := "SetupSecretInvalid"
		var migrationError *setupSecretMigrationError
		if errors.As(err, &migrationError) {
			reason = "SetupSecretMigrationRequired"
		}
		if statusErr := r.reportUnobservedFailure(ctx, &gameServer, reason, err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}
	configuration, err := gamemanagement.NormalizeConfiguration(definition.ID, gameServer.Spec.SelectedSetup.Configuration.Values.Raw)
	if err != nil {
		if statusErr := r.reportUnobservedFailure(ctx, &gameServer, "ConfigurationInvalid", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, nil
	}
	if err := r.validateModArtifacts(ctx, &gameServer, definition); err != nil {
		if statusErr := r.reportUnobservedFailure(ctx, &gameServer, "ModArtifactInvalid", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, nil
	}

	if _, err := r.ensurePVC(ctx, &gameServer, definition); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, &gameServer, "StorageReconcileFailed", err)
	}

	disk, err := r.reconcileManagedDisk(ctx, &gameServer, definition)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, &gameServer, "DiskJobReconcileFailed", err)
	}
	if disk.failed {
		return r.reportDiskJobFailure(ctx, &gameServer, definition, disk)
	}
	if disk.blocked {
		if gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerRunning {
			return r.reportStartingForDiskWork(ctx, &gameServer, definition, disk)
		}
		return r.reconcileStopped(ctx, &gameServer, definition)
	}

	if gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerStopped {
		return r.reconcileStopped(ctx, &gameServer, definition)
	}
	return r.reconcileRunning(ctx, &gameServer, definition, configuration, secretEnv, secretRevision)
}

func (r *GameServerReconciler) quiesceBeforeSecretValidation(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (bool, ctrl.Result, error) {
	deploymentDeleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return true, ctrl.Result{}, r.reportUnobservedFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	if deploymentDeleted == false {
		return false, ctrl.Result{}, nil
	}
	status := r.stoppingStatus(ctx, gameServer, fmt.Sprintf("Stopping the %s workload before validating sensitive configuration", definition.DisplayName))
	status.ObservedGeneration = gameServer.Status.ObservedGeneration
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadStopping", fmt.Sprintf("%s is being stopped before the replacement setup Secret is acknowledged", definition.DisplayName))
	if err := r.updateStatus(ctx, gameServer, status); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *GameServerReconciler) validateSetupSecret(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (map[string][]byte, int64, error) {
	setup := gameServer.Spec.SelectedSetup
	schema, ok := gamemanagement.Schema(definition.ID)
	if !ok {
		return nil, 0, fmt.Errorf("game %q has no management schema", definition.ID)
	}
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: setup.Configuration.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return nil, 0, fmt.Errorf("read referenced setup Secret %q: %w", key.Name, err)
	}
	if secret.Labels[plexusv1alpha1.LabelServerID] != gameServer.Spec.ServerID ||
		secret.Labels[plexusv1alpha1.LabelOwnerUserID] != gameServer.Spec.OwnerUserID ||
		secret.Labels[plexusv1alpha1.LabelGameID] != setup.GameID || secret.Labels[plexusv1alpha1.LabelSetupID] != setup.ID {
		return nil, 0, fmt.Errorf("referenced setup Secret %q has different ownership", key.Name)
	}
	if secret.Annotations[gamemanagement.SecretSchemaAnnotation] != schema.Secrets.Version {
		return nil, 0, &setupSecretMigrationError{name: key.Name, schemaVersion: secret.Annotations[gamemanagement.SecretSchemaAnnotation], supported: schema.Secrets.Version}
	}
	revision, err := strconv.ParseInt(secret.Annotations[gamemanagement.SecretRevisionAnnotation], 10, 64)
	if err != nil || revision < 1 {
		return nil, 0, fmt.Errorf("referenced setup Secret %q has an invalid revision", key.Name)
	}
	if secret.Immutable == nil || *secret.Immutable == false {
		return nil, 0, fmt.Errorf("referenced setup Secret %q must be immutable", key.Name)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return nil, 0, fmt.Errorf("referenced setup Secret %q must use type Opaque", key.Name)
	}
	if len(secret.Data) != 1 {
		return nil, 0, fmt.Errorf("referenced setup Secret %q has unexpected data keys", key.Name)
	}
	secretEnv, err := gamemanagement.RuntimeSecretEnv(definition.ID, secret.Data[gamemanagement.SecretDataKey])
	if err != nil {
		return nil, 0, fmt.Errorf("referenced setup Secret %q does not match the pinned adapter schema", key.Name)
	}
	return secretEnv, revision, nil
}

func managesMods(definition games.GameDefinition) bool {
	return definition.Workload.SupportsMods || definition.Workload.WorkshopStartup
}

func (r *GameServerReconciler) validateModArtifacts(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) error {
	mods := gameServer.Spec.SelectedSetup.Mods
	if managesMods(definition) == false {
		if len(mods) != 0 {
			return fmt.Errorf("%s does not support managed mods", definition.DisplayName)
		}
		return nil
	}
	if len(mods) > 1 {
		return fmt.Errorf("%s accepts at most one enabled mod selection", definition.DisplayName)
	}
	if definition.Workload.WorkshopStartup {
		return validateWorkshopMods(mods)
	}
	for _, mod := range mods {
		release := factorio.ModRelease{ProviderID: mod.ProviderID, ProviderModID: mod.ProviderModID, Name: mod.Name, Version: mod.Version, GameVersion: mod.GameVersion, Dependencies: mod.Dependencies}
		if err := factorio.ValidateModRelease(release); err != nil {
			return err
		}
		if mod.ArchiveFileName != mod.Name+"_"+mod.Version+".zip" || strings.TrimSpace(mod.ArtifactRef) == "" {
			return fmt.Errorf("mod artifact identity is invalid")
		}
		secret := &corev1.Secret{}
		key := client.ObjectKey{Namespace: gameServer.Namespace, Name: mod.ArtifactRef}
		if err := r.Get(ctx, key, secret); err != nil {
			return fmt.Errorf("read staged mod artifact %q: %w", key.Name, err)
		}
		if secret.Labels[plexusv1alpha1.LabelServerID] != gameServer.Spec.ServerID || secret.Labels[plexusv1alpha1.LabelOwnerUserID] != gameServer.Spec.OwnerUserID ||
			secret.Labels[plexusv1alpha1.LabelGameID] != gameServer.Spec.SelectedSetup.GameID || secret.Labels[plexusv1alpha1.LabelSetupID] != gameServer.Spec.SelectedSetup.ID {
			return fmt.Errorf("staged mod artifact %q has different ownership", key.Name)
		}
		if err := ensureControlledBy(gameServer, secret); err != nil {
			return fmt.Errorf("staged mod artifact %q has different controller ownership", key.Name)
		}
		if secret.Annotations[factorio.ModProviderAnnotation] != mod.ProviderID || secret.Annotations[factorio.ModIDAnnotation] != mod.ProviderModID ||
			secret.Annotations[factorio.ModVersionAnnotation] != mod.Version || secret.Annotations[factorio.ModSHA256Annotation] != mod.ArchiveSHA256 {
			return fmt.Errorf("staged mod artifact %q metadata does not match desired selection", key.Name)
		}
		if secret.Immutable == nil || *secret.Immutable == false || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 {
			return fmt.Errorf("staged mod artifact %q must be an immutable single-file artifact", key.Name)
		}
		if err := factorio.ValidateModArchive(release, secret.Data[factorio.ModArtifactDataKey], mod.ArchiveSHA256); err != nil {
			return fmt.Errorf("staged mod artifact %q: %w", key.Name, err)
		}
	}
	return nil
}

func validateWorkshopMods(mods []plexusv1alpha1.ModSpec) error {
	for _, mod := range mods {
		if strings.TrimSpace(mod.ArtifactRef) != "" || strings.TrimSpace(mod.ArchiveSHA256) != "" {
			return fmt.Errorf("Steam Workshop selection must not include a staged archive")
		}
		if err := zomboid.ValidateModRelease(workshopRelease(mod)); err != nil {
			return err
		}
	}
	return nil
}

func workshopRelease(mod plexusv1alpha1.ModSpec) zomboid.ModRelease {
	return zomboid.ModRelease{
		ProviderID:    mod.ProviderID,
		ProviderModID: mod.ProviderModID,
		Name:          mod.Name,
		Version:       mod.Version,
		GameVersion:   mod.GameVersion,
		Dependencies:  append([]string(nil), mod.Dependencies...),
		LoadIDs:       append([]string(nil), mod.LoadIDs...),
	}
}

type setupSecretMigrationError struct {
	name          string
	schemaVersion string
	supported     string
}

func (err *setupSecretMigrationError) Error() string {
	return fmt.Sprintf("referenced setup Secret %q uses schema %q; publish a replacement using supported schema %q", err.name, err.schemaVersion, err.supported)
}

func (r *GameServerReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&plexusv1alpha1.GameServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Owns(&batchv1.Job{}).
		Watches(&plexusv1alpha1.SaveImport{}, handler.EnqueueRequestsFromMapFunc(mapServerOwnedRequest)).
		Watches(&plexusv1alpha1.SaveExport{}, handler.EnqueueRequestsFromMapFunc(mapServerOwnedRequest)).
		Complete(r)
}

func (r *GameServerReconciler) reconcileRunning(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, configuration json.RawMessage, secretEnv map[string][]byte, secretRevision int64) (ctrl.Result, error) {
	if _, err := r.ensureConfigMap(ctx, gameServer, definition, configuration); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ConfigurationReconcileFailed", err)
	}
	if _, err := r.ensureRuntimeSecret(ctx, gameServer, definition, secretEnv, secretRevision); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ConfigurationReconcileFailed", err)
	}
	replaced, err := r.replaceServiceIfGameChanged(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceReconcileFailed", err)
	}
	if replaced {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	service, err := r.ensureService(ctx, gameServer, definition)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceReconcileFailed", err)
	}
	deployment, err := r.ensureDeployment(ctx, gameServer, definition, configuration, secretRevision)
	if err != nil {
		reason := "WorkloadReconcileFailed"
		if isWorkloadNameCollision(err) {
			return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportPermanentFailure(ctx, gameServer, "WorkloadNameCollision", err)
		}
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, reason, err)
	}
	if failure, err := r.workloadFailure(ctx, gameServer, definition, deployment); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadObservationFailed", err)
	} else if failure != nil {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseFailed, failure.message)
		preserveActiveRevision(&status, gameServer.Status)
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, failure.reason, status.Message)
		if failure.reason == "ModInstallFailed" || failure.reason == "SaveImportFailed" {
			setCondition(&status, gameServer.Generation, conditionMods, metav1.ConditionFalse, failure.reason, status.Message)
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
	}

	endpoint, endpointReady := serviceEndpoint(service, definition.Ports[0])
	if deploymentRolloutAvailable(deployment) == false {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, fmt.Sprintf("Waiting for the %s workload to become available", definition.DisplayName))
		preserveActiveRevision(&status, gameServer.Status)
		status.Endpoint = endpoint
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadUnavailable", fmt.Sprintf("Persistent storage and service are ready; the %s workload is not yet available", definition.DisplayName))
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
		setEndpointCondition(&status, gameServer.Generation, endpointReady)
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
	}
	if !endpointReady {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, fmt.Sprintf("%s is running; waiting for a public service endpoint", definition.DisplayName))
		acknowledgeActiveRevision(&status, gameServer, secretRevision)
		acknowledgeInstalledMods(&status, gameServer)
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "EndpointPending", fmt.Sprintf("%s is available, but the load balancer has not assigned a public endpoint", definition.DisplayName))
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
		setEndpointCondition(&status, gameServer.Generation, false)
		if managesMods(definition) {
			setCondition(&status, gameServer.Generation, conditionMods, metav1.ConditionTrue, "ModsInstalled", fmt.Sprintf("Enabled %s mod selection was installed by the available workload", definition.DisplayName))
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
	}

	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseRunning, fmt.Sprintf("%s workload is running", definition.DisplayName))
	acknowledgeActiveRevision(&status, gameServer, secretRevision)
	acknowledgeInstalledMods(&status, gameServer)
	status.Endpoint = endpoint
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionTrue, "WorkloadAvailable", fmt.Sprintf("%s workload is available", definition.DisplayName))
	setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
	setEndpointCondition(&status, gameServer.Generation, true)
	if managesMods(definition) {
		setCondition(&status, gameServer.Generation, conditionMods, metav1.ConditionTrue, "ModsInstalled", fmt.Sprintf("Enabled %s mod selection was installed by the available workload", definition.DisplayName))
	}
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func acknowledgeActiveRevision(status *plexusv1alpha1.GameServerStatus, gameServer *plexusv1alpha1.GameServer, secretRevision int64) {
	status.ActiveSetupID = gameServer.Spec.SelectedSetup.ID
	status.ObservedRestartGeneration = gameServer.Spec.RestartGeneration
	status.ObservedConfigurationGeneration = gameServer.Generation
	status.ObservedSecretRevision = secretRevision
}

func preserveActiveRevision(status *plexusv1alpha1.GameServerStatus, previous plexusv1alpha1.GameServerStatus) {
	status.ActiveSetupID = previous.ActiveSetupID
	status.ObservedConfigurationGeneration = previous.ObservedConfigurationGeneration
	status.ObservedSecretRevision = previous.ObservedSecretRevision
	status.InstalledMods = append([]plexusv1alpha1.InstalledMod(nil), previous.InstalledMods...)
	status.InstalledModsGeneration = previous.InstalledModsGeneration
}

func acknowledgeInstalledMods(status *plexusv1alpha1.GameServerStatus, gameServer *plexusv1alpha1.GameServer) {
	status.InstalledMods = observedInstalledMods(gameServer.Spec.SelectedSetup.Mods)
	status.InstalledModsGeneration = gameServer.Generation
}

func preserveInstalledMods(status *plexusv1alpha1.GameServerStatus, previous plexusv1alpha1.GameServerStatus) {
	status.InstalledMods = append([]plexusv1alpha1.InstalledMod(nil), previous.InstalledMods...)
	status.InstalledModsGeneration = previous.InstalledModsGeneration
}

func (r *GameServerReconciler) reportStartingForDiskWork(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, disk diskReconcile) (ctrl.Result, error) {
	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, disk.message)
	preserveInstalledMods(&status, gameServer.Status)
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, disk.reason, disk.message)
	setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
	setCondition(&status, gameServer.Generation, conditionDiskJob, metav1.ConditionFalse, disk.reason, disk.message)
	if managesMods(definition) {
		setCondition(&status, gameServer.Generation, conditionMods, metav1.ConditionFalse, disk.reason, disk.message)
	}
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) reportDiskJobFailure(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, disk diskReconcile) (ctrl.Result, error) {
	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseFailed, disk.message)
	preserveInstalledMods(&status, gameServer.Status)
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, disk.reason, disk.message)
	setCondition(&status, gameServer.Generation, conditionDiskJob, metav1.ConditionFalse, disk.reason, disk.message)
	if managesMods(definition) || disk.reason == "ModInstallFailed" {
		setCondition(&status, gameServer.Generation, conditionMods, metav1.ConditionFalse, disk.reason, disk.message)
	}
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func observedInstalledMods(mods []plexusv1alpha1.ModSpec) []plexusv1alpha1.InstalledMod {
	installed := make([]plexusv1alpha1.InstalledMod, 0, len(mods))
	for _, mod := range mods {
		installed = append(installed, plexusv1alpha1.InstalledMod{ProviderID: mod.ProviderID, ProviderModID: mod.ProviderModID, Name: mod.Name, Version: mod.Version})
	}
	return installed
}

type observedWorkloadFailure struct{ reason, message string }

func (r *GameServerReconciler) workloadFailure(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, deployment *appsv1.Deployment) (*observedWorkloadFailure, error) {
	pods, err := r.ownedDeploymentPods(ctx, gameServer, deployment)
	if err != nil {
		return nil, err
	}
	for _, pod := range pods {
		for _, condition := range pod.Status.Conditions {
			if condition.Type == corev1.PodScheduled && condition.Status == corev1.ConditionFalse && condition.Reason == corev1.PodReasonUnschedulable {
				return &observedWorkloadFailure{reason: "WorkloadSchedulingFailed", message: definition.DisplayName + " workload could not be scheduled"}, nil
			}
		}
		for _, status := range append(append([]corev1.ContainerStatus(nil), pod.Status.InitContainerStatuses...), pod.Status.ContainerStatuses...) {
			if status.State.Waiting != nil && imageFailureReason(status.State.Waiting.Reason) {
				return &observedWorkloadFailure{reason: "WorkloadImagePullFailed", message: definition.DisplayName + " workload image could not be pulled"}, nil
			}
			if status.State.Terminated == nil || status.State.Terminated.ExitCode == 0 {
				continue
			}
			return &observedWorkloadFailure{reason: "WorkloadInitializationFailed", message: definition.DisplayName + " workload initialization failed"}, nil
		}
	}
	for _, condition := range deployment.Status.Conditions {
		if condition.Status != corev1.ConditionTrue {
			continue
		}
		if condition.Type == appsv1.DeploymentReplicaFailure || (condition.Type == appsv1.DeploymentProgressing && condition.Reason == "ProgressDeadlineExceeded") {
			return &observedWorkloadFailure{reason: "WorkloadRolloutFailed", message: definition.DisplayName + " workload rollout failed"}, nil
		}
	}
	return nil, nil
}

func imageFailureReason(reason string) bool {
	return reason == "ErrImagePull" || reason == "ImagePullBackOff" || reason == "InvalidImageName"
}

func (r *GameServerReconciler) ownedDeploymentPods(ctx context.Context, gameServer *plexusv1alpha1.GameServer, deployment *appsv1.Deployment) ([]corev1.Pod, error) {
	var podList corev1.PodList
	if err := r.List(ctx, &podList, client.InNamespace(gameServer.Namespace), client.MatchingLabels(selectorLabels(gameServer))); err != nil {
		return nil, err
	}
	owned := make([]corev1.Pod, 0, len(podList.Items))
	for _, pod := range podList.Items {
		if pod.Annotations["plexus.gg/configuration-generation"] != fmt.Sprint(gameServer.Generation) {
			continue
		}
		controller := metav1.GetControllerOf(&pod)
		if controller == nil || controller.Kind != "ReplicaSet" {
			continue
		}
		var replicaSet appsv1.ReplicaSet
		if err := r.Get(ctx, client.ObjectKey{Namespace: pod.Namespace, Name: controller.Name}, &replicaSet); err != nil {
			if client.IgnoreNotFound(err) == nil {
				continue
			}
			return nil, err
		}
		replicaSetController := metav1.GetControllerOf(&replicaSet)
		if replicaSetController != nil && replicaSetController.Kind == "Deployment" && replicaSetController.UID == deployment.UID {
			owned = append(owned, pod)
		}
	}
	return owned, nil
}

func deploymentRolloutAvailable(deployment *appsv1.Deployment) bool {
	desiredReplicas := int32(1)
	if deployment.Spec.Replicas != nil {
		desiredReplicas = *deployment.Spec.Replicas
	}
	return deployment.Status.ObservedGeneration == deployment.Generation &&
		deployment.Status.Replicas == desiredReplicas && deployment.Status.UpdatedReplicas == desiredReplicas &&
		deployment.Status.AvailableReplicas == desiredReplicas
}

func (r *GameServerReconciler) ensureRuntimeSecret(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, secretEnv map[string][]byte, revision int64) (*corev1.Secret, error) {
	name := runtimeSecretName(gameServer, revision)
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: name}
	desiredData := secretEnv
	if desiredData == nil {
		return nil, fmt.Errorf("game %q produced no runtime secret environment", definition.ID)
	}
	if err := r.Get(ctx, key, secret); err == nil {
		if err := ensureControlledBy(gameServer, secret); err != nil {
			return nil, err
		}
		if secret.Immutable == nil || *secret.Immutable == false || secret.Type != corev1.SecretTypeOpaque || !reflect.DeepEqual(secret.Data, desiredData) {
			return nil, fmt.Errorf("owned runtime Secret %q does not match its immutable revision", name)
		}
		return secret, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}

	immutable := true
	secret = &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name: name, Namespace: gameServer.Namespace,
			Labels:      childLabels(gameServer),
			Annotations: map[string]string{gamemanagement.SecretRevisionAnnotation: strconv.FormatInt(revision, 10)},
		},
		Immutable: &immutable,
		Type:      corev1.SecretTypeOpaque,
		Data:      desiredData,
	}
	if err := controllerutil.SetControllerReference(gameServer, secret, r.Scheme); err != nil {
		return nil, err
	}
	return secret, r.Create(ctx, secret)
}

func (r *GameServerReconciler) ensureConfigMap(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, configuration json.RawMessage) (*corev1.ConfigMap, error) {
	desiredData, err := gamemanagement.RenderConfigFiles(definition.ID, configuration)
	if err != nil {
		return nil, err
	}
	name := runtimeConfigMapName(gameServer)
	configMap := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: name}
	if err := r.Get(ctx, key, configMap); err == nil {
		if err := ensureControlledBy(gameServer, configMap); err != nil {
			return nil, err
		}
		if configMap.Immutable == nil || *configMap.Immutable == false || !reflect.DeepEqual(configMap.Data, desiredData) {
			return nil, fmt.Errorf("owned runtime ConfigMap %q does not match its immutable generation", name)
		}
		return configMap, nil
	} else if client.IgnoreNotFound(err) != nil {
		return nil, err
	}

	immutable := true
	configMap = &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gameServer.Namespace, Labels: childLabels(gameServer)},
		Immutable:  &immutable,
		Data:       desiredData,
	}
	if err := controllerutil.SetControllerReference(gameServer, configMap, r.Scheme); err != nil {
		return nil, err
	}
	return configMap, r.Create(ctx, configMap)
}

func (r *GameServerReconciler) reconcileStopped(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (ctrl.Result, error) {
	deleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	replaced, err := r.replaceServiceIfGameChanged(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceCleanupFailed", err)
	}
	if deleted {
		status := r.stoppingStatus(ctx, gameServer, fmt.Sprintf("Stopping the %s workload; persistent storage is retained", definition.DisplayName))
		preserveInstalledMods(&status, gameServer.Status)
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadStopping", definition.DisplayName+" workload is being removed")
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is retained")
		if err := r.updateStatus(ctx, gameServer, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}
	if replaced {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopped, definition.DisplayName+" workload is stopped; persistent storage is retained")
	preserveInstalledMods(&status, gameServer.Status)
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "DesiredStopped", "No "+definition.DisplayName+" workload is running")
	setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is retained")
	setCondition(&status, gameServer.Generation, conditionEndpoint, metav1.ConditionFalse, "DesiredStopped", "A stopped server has no public endpoint")
	setCondition(&status, gameServer.Generation, conditionShutdown, metav1.ConditionTrue, "DesiredStopped", "No "+definition.DisplayName+" workload is running")
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) reconcileUnloaded(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (ctrl.Result, error) {
	deleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	if deleted {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopping, "Unloading the server workload; persistent storage is retained")
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadStopping", "Runtime resources are being removed")
		if err := r.updateStatus(ctx, gameServer, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopped, "Server is unloaded and stopped; existing persistent storage is retained")
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "DesiredStopped", "No game workload is running")
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) ensureMetadata(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	before := gameServer.DeepCopy()
	if gameServer.Labels == nil {
		gameServer.Labels = map[string]string{}
	}
	gameServer.Labels[plexusv1alpha1.LabelServerID] = gameServer.Spec.ServerID
	gameServer.Labels[plexusv1alpha1.LabelOwnerUserID] = gameServer.Spec.OwnerUserID
	if gameServer.Spec.SelectedSetup == nil {
		delete(gameServer.Labels, plexusv1alpha1.LabelGameID)
	} else {
		gameServer.Labels[plexusv1alpha1.LabelGameID] = gameServer.Spec.SelectedSetup.GameID
	}
	controllerutil.AddFinalizer(gameServer, GameServerFinalizer)
	if reflect.DeepEqual(before.Labels, gameServer.Labels) && reflect.DeepEqual(before.Finalizers, gameServer.Finalizers) {
		return false, nil
	}
	return true, r.Patch(ctx, gameServer, client.MergeFrom(before))
}

func (r *GameServerReconciler) ensurePVC(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (*corev1.PersistentVolumeClaim, error) {
	pvc := &corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, pvc, func() error {
		if pvc.ResourceVersion != "" {
			if err := ensureControlledBy(gameServer, pvc); err != nil {
				return err
			}
		}
		applyLabels(&pvc.ObjectMeta, childLabels(gameServer))
		if err := controllerutil.SetControllerReference(gameServer, pvc, r.Scheme); err != nil {
			return err
		}
		pvc.Spec.AccessModes = []corev1.PersistentVolumeAccessMode{corev1.ReadWriteOnce}
		required := resource.MustParse(fmt.Sprintf("%dGi", games.CalculateDiskSize(definition.ID, 0, 0)))
		current := pvc.Spec.Resources.Requests.Storage()
		if current == nil || current.Cmp(required) < 0 {
			if pvc.Spec.Resources.Requests == nil {
				pvc.Spec.Resources.Requests = corev1.ResourceList{}
			}
			pvc.Spec.Resources.Requests[corev1.ResourceStorage] = required
		}
		return nil
	})
	return pvc, err
}

func (r *GameServerReconciler) ensureService(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (*corev1.Service, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, service, func() error {
		if service.ResourceVersion != "" {
			if err := ensureControlledBy(gameServer, service); err != nil {
				return err
			}
		}
		applyLabels(&service.ObjectMeta, childLabels(gameServer))
		if err := controllerutil.SetControllerReference(gameServer, service, r.Scheme); err != nil {
			return err
		}
		service.Spec.Type = corev1.ServiceTypeLoadBalancer
		service.Spec.Selector = selectorLabels(gameServer)
		service.Spec.Ports = servicePorts(definition)
		return nil
	})
	return service, err
}

// replaceServiceIfGameChanged deletes the owned Service when it belongs to a
// different game. Published WAN ports stay assigned for the life of one Service
// UID; a game switch therefore allocates new endpoints by replacing it.
func (r *GameServerReconciler) replaceServiceIfGameChanged(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	if gameServer.Spec.SelectedSetup == nil {
		return false, nil
	}
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if service.Labels[plexusv1alpha1.LabelGameID] == gameServer.Spec.SelectedSetup.GameID {
		return false, nil
	}
	return r.deleteService(ctx, gameServer)
}

func (r *GameServerReconciler) ensureDeployment(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, configuration json.RawMessage, secretRevision int64) (*appsv1.Deployment, error) {
	lifecycle, terminationGracePeriod, err := gracefulShutdownLifecycle(definition)
	if err != nil {
		return nil, err
	}
	env, err := environment(definition, configuration, runtimeSecretName(gameServer, secretRevision), gameServer)
	if err != nil {
		return nil, err
	}
	workload := definition.Workload
	initMounts := []corev1.VolumeMount{
		{Name: workload.Config.SourceName, MountPath: workload.Config.SourcePath, ReadOnly: true},
		{Name: dataVolumeName, MountPath: workload.DataMountPath},
	}
	if workload.Config.VolumeName != "" {
		initMounts[1] = corev1.VolumeMount{Name: workload.Config.VolumeName, MountPath: workload.Config.MountPath}
	}
	initContainers := []corev1.Container{{
		Name:         workload.Config.InitName,
		Image:        definition.DefaultImage,
		Command:      []string{"/bin/sh", "-c"},
		Args:         []string{workload.Config.InitCopyCommand},
		VolumeMounts: initMounts,
	}}
	volumeMounts := []corev1.VolumeMount{
		{Name: dataVolumeName, MountPath: workload.DataMountPath},
	}
	if workload.Config.VolumeName != "" {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: workload.Config.VolumeName, MountPath: workload.Config.MountPath})
	}
	for _, mount := range workload.AdditionalMounts {
		volumeMounts = append(volumeMounts, corev1.VolumeMount{Name: mount.Name, MountPath: mount.MountPath, SubPath: mount.SubPath})
	}
	name, err := workloadDeploymentName(gameServer)
	if err != nil {
		return nil, err
	}
	if err := r.ensureWorkloadNameAvailable(ctx, gameServer, name); err != nil {
		return nil, err
	}
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: gameServer.Namespace}}
	_, err = controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
		if deployment.ResourceVersion != "" {
			if err := ensureControlledBy(gameServer, deployment); err != nil {
				return err
			}
		}
		applyLabels(&deployment.ObjectMeta, childLabels(gameServer))
		if err := controllerutil.SetControllerReference(gameServer, deployment, r.Scheme); err != nil {
			return err
		}
		replicas := int32(1)
		deployment.Spec.Replicas = &replicas
		deployment.Spec.Strategy = appsv1.DeploymentStrategy{Type: appsv1.RecreateDeploymentStrategyType}
		deployment.Spec.Selector = &metav1.LabelSelector{MatchLabels: selectorLabels(gameServer)}
		deployment.Spec.Template = corev1.PodTemplateSpec{
			ObjectMeta: metav1.ObjectMeta{
				Labels: childLabels(gameServer),
				Annotations: map[string]string{
					"plexus.gg/restart-generation":          fmt.Sprint(gameServer.Spec.RestartGeneration),
					"plexus.gg/configuration-generation":    fmt.Sprint(gameServer.Generation),
					gamemanagement.SecretRevisionAnnotation: strconv.FormatInt(secretRevision, 10),
				},
			},
			Spec: corev1.PodSpec{
				TerminationGracePeriodSeconds: terminationGracePeriod,
				ImagePullSecrets:              ghcrImagePullSecrets(),
				InitContainers:                initContainers,
				Containers: []corev1.Container{{
					Name:         workload.ContainerName,
					Image:        definition.DefaultImage,
					Env:          env,
					Ports:        containerPorts(definition),
					VolumeMounts: volumeMounts,
					Lifecycle:    lifecycle,
				}},
				Volumes: workloadVolumes(gameServer, definition),
			},
		}
		return nil
	})
	if err != nil {
		return deployment, err
	}
	if err := r.deleteStaleWorkloadDeployments(ctx, gameServer, name); err != nil {
		return deployment, err
	}
	return deployment, nil
}

func ghcrImagePullSecrets() []corev1.LocalObjectReference {
	return []corev1.LocalObjectReference{{Name: "docker-secret"}}
}

func gracefulShutdownLifecycle(definition games.GameDefinition) (*corev1.Lifecycle, *int64, error) {
	policy := definition.Shutdown
	if policy.TimeoutSeconds < 1 {
		return nil, nil, fmt.Errorf("game %q has no supported graceful shutdown policy", definition.ID)
	}
	timeout := int64(policy.TimeoutSeconds)
	if definition.Workload.Supervisor {
		return nil, &timeout, nil
	}
	switch policy.Strategy {
	case "rcon-command":
		if policy.Command == "" {
			return nil, nil, fmt.Errorf("game %q has no supported graceful shutdown policy", definition.ID)
		}
		return &corev1.Lifecycle{
			PreStop: &corev1.LifecycleHandler{Exec: &corev1.ExecAction{Command: []string{"rcon", policy.Command}}},
		}, &timeout, nil
	case "process-signal":
		return nil, &timeout, nil
	default:
		return nil, nil, fmt.Errorf("game %q has no supported graceful shutdown policy", definition.ID)
	}
}

func workloadVolumes(gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) []corev1.Volume {
	volumes := []corev1.Volume{
		{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: gameServer.Name}}},
		{Name: definition.Workload.Config.SourceName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: runtimeConfigMapName(gameServer)}}}},
	}
	if definition.Workload.Config.VolumeName != "" {
		volumes = append(volumes, corev1.Volume{Name: definition.Workload.Config.VolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}})
	}
	return volumes
}

func modArtifactVolumes(gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) []corev1.Volume {
	if definition.Workload.SupportsMods == false || len(gameServer.Spec.SelectedSetup.Mods) != 1 {
		return nil
	}
	return []corev1.Volume{{Name: modSourceName, VolumeSource: corev1.VolumeSource{Secret: &corev1.SecretVolumeSource{
		SecretName: gameServer.Spec.SelectedSetup.Mods[0].ArtifactRef,
		Items:      []corev1.KeyToPath{{Key: factorio.ModArtifactDataKey, Path: factorio.ModArtifactDataKey}},
	}}}}
}
func (r *GameServerReconciler) reconcileDelete(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(gameServer, GameServerFinalizer) {
		return ctrl.Result{}, nil
	}

	remaining := false
	deployments, err := r.ownedWorkloadDeployments(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, err
	}
	var owned []client.Object
	for i := range deployments {
		owned = append(owned, &deployments[i])
	}
	owned = append(owned,
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}},
		&batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: diskJobName(gameServer), Namespace: gameServer.Namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}},
	)
	for _, object := range owned {
		exists, err := r.deleteOwnedObject(ctx, gameServer, object)
		if err != nil {
			return ctrl.Result{}, err
		}
		remaining = remaining || exists
	}
	runtimeInputsRemaining, err := r.deleteOwnedRuntimeInputs(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, err
	}
	remaining = remaining || runtimeInputsRemaining
	if remaining {
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	before := gameServer.DeepCopy()
	controllerutil.RemoveFinalizer(gameServer, GameServerFinalizer)
	return ctrl.Result{}, r.Patch(ctx, gameServer, client.MergeFrom(before))
}

func (r *GameServerReconciler) deleteOwnedRuntimeInputs(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	remaining := false
	var configMaps corev1.ConfigMapList
	if err := r.List(ctx, &configMaps, client.InNamespace(gameServer.Namespace)); err != nil {
		return false, err
	}
	for index := range configMaps.Items {
		if !metav1.IsControlledBy(&configMaps.Items[index], gameServer) {
			continue
		}
		exists, err := r.deleteOwnedObject(ctx, gameServer, &configMaps.Items[index])
		if err != nil {
			return false, err
		}
		remaining = remaining || exists
	}

	var secrets corev1.SecretList
	if err := r.List(ctx, &secrets, client.InNamespace(gameServer.Namespace)); err != nil {
		return false, err
	}
	for index := range secrets.Items {
		if !metav1.IsControlledBy(&secrets.Items[index], gameServer) {
			continue
		}
		exists, err := r.deleteOwnedObject(ctx, gameServer, &secrets.Items[index])
		if err != nil {
			return false, err
		}
		remaining = remaining || exists
	}
	return remaining, nil
}

func runtimeConfigMapName(gameServer *plexusv1alpha1.GameServer) string {
	return revisionScopedResourceName(gameServer.Name, fmt.Sprintf("-config-g%d", gameServer.Generation))
}

func runtimeSecretName(gameServer *plexusv1alpha1.GameServer, secretRevision int64) string {
	return revisionScopedResourceName(gameServer.Name, fmt.Sprintf("-runtime-g%d-r%d", gameServer.Generation, secretRevision))
}

func revisionScopedResourceName(gameServerName string, suffix string) string {
	normalized := strings.ReplaceAll(gameServerName, ".", "-")
	candidate := normalized + suffix
	if normalized == gameServerName && len(candidate) <= 63 {
		return candidate
	}
	digest := sha256.Sum256([]byte(gameServerName))
	hashSuffix := fmt.Sprintf("-%x", digest[:4])
	maxPrefixLength := 63 - len(hashSuffix) - len(suffix)
	if len(normalized) > maxPrefixLength {
		normalized = normalized[:maxPrefixLength]
	}
	normalized = strings.TrimRight(normalized, "-")
	return normalized + hashSuffix + suffix
}

func (r *GameServerReconciler) deleteOwnedObject(ctx context.Context, gameServer *plexusv1alpha1.GameServer, object client.Object) (bool, error) {
	if err := r.Get(ctx, client.ObjectKeyFromObject(object), object); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if err := ensureControlledBy(gameServer, object); err != nil {
		return false, err
	}
	if object.GetDeletionTimestamp() != nil {
		return true, nil
	}
	return true, r.Delete(ctx, object)
}

func (r *GameServerReconciler) deleteDeployment(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	deployments, err := r.ownedWorkloadDeployments(ctx, gameServer)
	if err != nil {
		return false, err
	}
	remaining := false
	for i := range deployments {
		exists, err := r.deleteOwnedObject(ctx, gameServer, &deployments[i])
		if err != nil {
			return remaining, err
		}
		remaining = remaining || exists
	}
	if remaining {
		if _, err := r.forceDeletePods(ctx, gameServer); err != nil {
			return true, err
		}
		return true, nil
	}
	return r.forceDeletePods(ctx, gameServer)
}

func (r *GameServerReconciler) forceDeletePods(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	if gameServer.Spec.ShutdownMode != plexusv1alpha1.ShutdownModeForce {
		return false, nil
	}
	if gameServer.UID == "" {
		return false, fmt.Errorf("GameServer %s/%s has no UID; refusing to force-delete pods", gameServer.Namespace, gameServer.Name)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(gameServer.Namespace), client.MatchingLabels(forceDeletePodLabels(gameServer))); err != nil {
		return false, err
	}
	for index := range pods.Items {
		if err := r.Delete(ctx, &pods.Items[index], client.GracePeriodSeconds(0)); client.IgnoreNotFound(err) != nil {
			return true, err
		}
	}
	return len(pods.Items) > 0, nil
}

func (r *GameServerReconciler) deleteService(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, error) {
	service := &corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(service), service); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if err := ensureControlledBy(gameServer, service); err != nil {
		return false, err
	}
	if !service.DeletionTimestamp.IsZero() {
		return true, nil
	}
	return true, r.Delete(ctx, service)
}

func (r *GameServerReconciler) reportFailure(ctx context.Context, gameServer *plexusv1alpha1.GameServer, reason string, reconcileErr error) error {
	if err := r.updateFailedStatus(ctx, gameServer, reason, reconcileErr); err != nil {
		return fmt.Errorf("%w; update failed status: %v", reconcileErr, err)
	}
	return reconcileErr
}

func (r *GameServerReconciler) reportPermanentFailure(ctx context.Context, gameServer *plexusv1alpha1.GameServer, reason string, reconcileErr error) error {
	return r.updateFailedStatus(ctx, gameServer, reason, reconcileErr)
}

func (r *GameServerReconciler) reportUnobservedFailure(ctx context.Context, gameServer *plexusv1alpha1.GameServer, reason string, reconcileErr error) error {
	status := gameServer.Status
	status.Conditions = append([]metav1.Condition(nil), gameServer.Status.Conditions...)
	if status.Phase == "" {
		status.Phase = plexusv1alpha1.GameServerPhaseFailed
	}
	status.Message = reconcileErr.Error()
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, reason, reconcileErr.Error())
	return r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) updateFailedStatus(ctx context.Context, gameServer *plexusv1alpha1.GameServer, reason string, reconcileErr error) error {
	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseFailed, reconcileErr.Error())
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, reason, reconcileErr.Error())
	if reason == "StorageReconcileFailed" {
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionFalse, reason, reconcileErr.Error())
	}
	return r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) updateStatus(ctx context.Context, gameServer *plexusv1alpha1.GameServer, status plexusv1alpha1.GameServerStatus) error {
	status.LastObservedAt = gameServer.Status.LastObservedAt
	if reflect.DeepEqual(gameServer.Status, status) {
		if status.LastObservedAt != nil && time.Since(status.LastObservedAt.Time) < observationRefreshInterval {
			return nil
		}
	}
	now := metav1.Now()
	status.LastObservedAt = &now
	gameServer.Status = status
	return r.Status().Update(ctx, gameServer)
}

func observedStatus(gameServer *plexusv1alpha1.GameServer, phase plexusv1alpha1.GameServerPhase, message string) plexusv1alpha1.GameServerStatus {
	return plexusv1alpha1.GameServerStatus{
		Phase:                     phase,
		ObservedGeneration:        gameServer.Generation,
		ObservedRestartGeneration: gameServer.Status.ObservedRestartGeneration,
		Conditions:                append([]metav1.Condition(nil), gameServer.Status.Conditions...),
		Message:                   message,
	}
}

func (r *GameServerReconciler) stoppingStatus(ctx context.Context, gameServer *plexusv1alpha1.GameServer, gracefulMessage string) plexusv1alpha1.GameServerStatus {
	message, reason := r.shutdownProgress(ctx, gameServer, gracefulMessage)
	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopping, message)
	setCondition(&status, gameServer.Generation, conditionShutdown, metav1.ConditionFalse, reason, message)
	return status
}

func (r *GameServerReconciler) shutdownProgress(ctx context.Context, gameServer *plexusv1alpha1.GameServer, gracefulMessage string) (string, string) {
	if gameServer.Spec.ShutdownMode == plexusv1alpha1.ShutdownModeForce {
		name := "game"
		if gameServer.Spec.SelectedSetup != nil {
			if definition, err := games.Get(gameServer.Spec.SelectedSetup.GameID); err == nil && definition.DisplayName != "" {
				name = definition.DisplayName
			}
		}
		return fmt.Sprintf("Force-stopping the %s workload; persistent storage is retained", name), "ForceStop"
	}
	if r.shutdownHasTimedOut(ctx, gameServer) {
		return takingLongerThanExpected, "TakingLongerThanExpected"
	}
	return gracefulMessage, "GracefulShutdown"
}

func (r *GameServerReconciler) shutdownHasTimedOut(ctx context.Context, gameServer *plexusv1alpha1.GameServer) bool {
	if gameServer.Spec.SelectedSetup == nil {
		return false
	}
	definition, err := games.Get(gameServer.Spec.SelectedSetup.GameID)
	if err != nil || definition.Shutdown.TimeoutSeconds < 1 {
		return false
	}
	startedAt := shutdownStartedAt(ctx, r.Client, gameServer)
	if startedAt.IsZero() {
		return false
	}
	return !startedAt.After(r.now().Add(-time.Duration(definition.Shutdown.TimeoutSeconds) * time.Second))
}

func shutdownStartedAt(ctx context.Context, kubeClient client.Client, gameServer *plexusv1alpha1.GameServer) time.Time {
	if condition := meta.FindStatusCondition(gameServer.Status.Conditions, conditionShutdown); condition != nil && condition.Status == metav1.ConditionFalse && !condition.LastTransitionTime.IsZero() {
		return condition.LastTransitionTime.Time
	}
	if gameServer.Spec.SelectedSetup == nil {
		return time.Time{}
	}
	name := gameServer.Name
	if workloadName, err := workloadDeploymentName(gameServer); err == nil {
		name = workloadName
	}
	deployment := &appsv1.Deployment{}
	if err := kubeClient.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: name}, deployment); err != nil {
		return time.Time{}
	}
	if deployment.DeletionTimestamp.IsZero() {
		return time.Time{}
	}
	return deployment.DeletionTimestamp.Time
}

func (r *GameServerReconciler) now() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

func setCondition(status *plexusv1alpha1.GameServerStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason string, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, ObservedGeneration: generation,
		Reason: reason, Message: message,
	})
}

func setEndpointCondition(status *plexusv1alpha1.GameServerStatus, generation int64, ready bool) {
	if ready {
		setCondition(status, generation, conditionEndpoint, metav1.ConditionTrue, "LoadBalancerReady", "Public endpoint is assigned")
		return
	}
	setCondition(status, generation, conditionEndpoint, metav1.ConditionFalse, "LoadBalancerPending", "Waiting for the load balancer to assign a public endpoint")
}

func validateDesiredState(gameServer *plexusv1alpha1.GameServer) error {
	if gameServer.Spec.SelectedSetup == nil {
		if gameServer.Spec.DesiredPower != plexusv1alpha1.DesiredPowerStopped {
			return fmt.Errorf("a GameServer without a selected setup must be stopped")
		}
		return nil
	}
	if gameServer.Spec.DesiredPower != plexusv1alpha1.DesiredPowerRunning && gameServer.Spec.DesiredPower != plexusv1alpha1.DesiredPowerStopped {
		return fmt.Errorf("unsupported desiredPower %q", gameServer.Spec.DesiredPower)
	}
	return nil
}

func ensureControlledBy(owner *plexusv1alpha1.GameServer, object metav1.Object) error {
	controller := metav1.GetControllerOf(object)
	if controller == nil || controller.UID != owner.UID {
		return fmt.Errorf("%s %s/%s is not controlled by GameServer %s", reflect.TypeOf(object).Elem().Name(), object.GetNamespace(), object.GetName(), owner.Name)
	}
	return nil
}

func childLabels(gameServer *plexusv1alpha1.GameServer) map[string]string {
	labels := selectorLabels(gameServer)
	labels[plexusv1alpha1.LabelGameServerUID] = string(gameServer.UID)
	labels[plexusv1alpha1.LabelOwnerUserID] = gameServer.Spec.OwnerUserID
	labels[plexusv1alpha1.LabelGameID] = gameServer.Spec.SelectedSetup.GameID
	labels[plexusv1alpha1.LabelSetupID] = gameServer.Spec.SelectedSetup.ID
	return labels
}

func forceDeletePodLabels(gameServer *plexusv1alpha1.GameServer) map[string]string {
	labels := selectorLabels(gameServer)
	labels[plexusv1alpha1.LabelGameServerUID] = string(gameServer.UID)
	return labels
}

func selectorLabels(gameServer *plexusv1alpha1.GameServer) map[string]string {
	return map[string]string{
		plexusv1alpha1.LabelServerID:  gameServer.Spec.ServerID,
		plexusv1alpha1.LabelComponent: plexusv1alpha1.ComponentGameServer,
	}
}

func applyLabels(metadata *metav1.ObjectMeta, labels map[string]string) {
	if metadata.Labels == nil {
		metadata.Labels = map[string]string{}
	}
	maps.Copy(metadata.Labels, labels)
}

func servicePorts(definition games.GameDefinition) []corev1.ServicePort {
	ports := make([]corev1.ServicePort, 0, len(definition.Ports))
	for _, port := range definition.Ports {
		ports = append(ports, corev1.ServicePort{Name: port.Name, Port: port.Port, Protocol: protocol(port.Protocol)})
	}
	return ports
}

func containerPorts(definition games.GameDefinition) []corev1.ContainerPort {
	ports := make([]corev1.ContainerPort, 0, len(definition.Ports))
	for _, port := range definition.Ports {
		ports = append(ports, corev1.ContainerPort{Name: port.Name, ContainerPort: port.Port, Protocol: protocol(port.Protocol)})
	}
	return ports
}

func environment(definition games.GameDefinition, configuration json.RawMessage, runtimeSecretName string, gameServer *plexusv1alpha1.GameServer) ([]corev1.EnvVar, error) {
	values := maps.Clone(definition.DefaultEnv)
	if values == nil {
		values = map[string]string{}
	}
	fromConfig, err := gamemanagement.ConfigurationEnv(definition.ID, configuration)
	if err != nil {
		return nil, err
	}
	for name, value := range fromConfig {
		values[name] = value
	}
	if definition.Workload.WorkshopStartup {
		var release *zomboid.ModRelease
		if setup := gameServer.Spec.SelectedSetup; setup != nil && len(setup.Mods) == 1 {
			decoded := workshopRelease(setup.Mods[0])
			release = &decoded
		}
		for name, value := range zomboid.RuntimeWorkshopEnv(release) {
			values[name] = value
		}
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]corev1.EnvVar, 0, len(names)+len(definition.Workload.SecretEnvKeys))
	for _, name := range names {
		environment = append(environment, corev1.EnvVar{Name: name, Value: values[name]})
	}
	for _, name := range definition.Workload.SecretEnvKeys {
		environment = append(environment, corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: runtimeSecretName}, Key: name,
			}},
		})
	}
	return environment, nil
}

func protocol(value string) corev1.Protocol {
	if value == string(corev1.ProtocolUDP) {
		return corev1.ProtocolUDP
	}
	return corev1.ProtocolTCP
}

func serviceEndpoint(service *corev1.Service, port games.GamePort) (string, bool) {
	for _, ingress := range service.Status.LoadBalancer.Ingress {
		if ingress.Hostname != "" {
			return fmt.Sprintf("%s:%d", ingress.Hostname, port.Port), true
		}
		if ingress.IP != "" {
			return fmt.Sprintf("%s:%d", ingress.IP, port.Port), true
		}
	}
	return "", false
}

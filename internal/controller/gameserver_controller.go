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
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/internal/games"
	factorio "github.com/AnthonyPoschen/plexus-controller/pkg/gamemanagement/factorio/v1"
)

const (
	// GameServerFinalizer keeps the GameServer present until its controller-owned
	// runtime resources have been removed.
	GameServerFinalizer = "plexus.gg/runtime-cleanup"

	conditionReady             = "Ready"
	conditionStorage           = "StorageReady"
	conditionEndpoint          = "EndpointReady"
	componentLabel             = "app.kubernetes.io/component"
	componentValue             = "game-server"
	dataVolumeName             = "game-data"
	dataMountPath              = "/factorio"
	configVolumeName           = "factorio-config"
	configSourceName           = "factorio-config-source"
	configFileName             = "server-settings.json"
	configMountPath            = "/factorio/config"
	configSourcePath           = "/plexus/config"
	observationRefreshInterval = 30 * time.Second
)

// GameServerReconciler turns Factorio GameServers into their persistent
// storage, network service, and desired running or stopped workload.
type GameServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=gameservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=services;persistentvolumeclaims,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups="",resources=configmaps,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;list;watch;create;delete

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
	if err != nil || definition.ID != factorio.GameID {
		if err == nil {
			err = fmt.Errorf("game %q does not have a workload reconciler", definition.ID)
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportPermanentFailure(ctx, &gameServer, "UnsupportedGame", err)
	}
	if gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerStopped {
		handled, result, err := r.quiesceBeforeSecretValidation(ctx, &gameServer)
		if err != nil || handled {
			return result, err
		}
	}
	if gameServer.Spec.SelectedSetup.Configuration.SchemaVersion != factorio.SchemaVersion {
		err := fmt.Errorf("Factorio setup schemaVersion is %q; migrate the setup to supported schema %q", gameServer.Spec.SelectedSetup.Configuration.SchemaVersion, factorio.SchemaVersion)
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.reportUnobservedFailure(ctx, &gameServer, "ConfigurationMigrationRequired", err)
	}
	secrets, secretRevision, err := r.validateSetupSecret(ctx, &gameServer)
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
	configuration, err := factorio.DecodeConfiguration(gameServer.Spec.SelectedSetup.Configuration.Values.Raw)
	if err != nil {
		if statusErr := r.reportUnobservedFailure(ctx, &gameServer, "ConfigurationInvalid", err); statusErr != nil {
			return ctrl.Result{}, statusErr
		}
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, nil
	}

	if _, err := r.ensurePVC(ctx, &gameServer, definition); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, &gameServer, "StorageReconcileFailed", err)
	}

	if gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerStopped {
		return r.reconcileStopped(ctx, &gameServer)
	}
	return r.reconcileRunning(ctx, &gameServer, definition, configuration, secrets, secretRevision)
}

func (r *GameServerReconciler) quiesceBeforeSecretValidation(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (bool, ctrl.Result, error) {
	deploymentDeleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return true, ctrl.Result{}, r.reportUnobservedFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	serviceDeleted, err := r.deleteService(ctx, gameServer)
	if err != nil {
		return true, ctrl.Result{}, r.reportUnobservedFailure(ctx, gameServer, "ServiceCleanupFailed", err)
	}
	if deploymentDeleted == false && serviceDeleted == false {
		return false, ctrl.Result{}, nil
	}
	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopping, "Stopping the Factorio workload before validating sensitive configuration")
	status.ObservedGeneration = gameServer.Status.ObservedGeneration
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadStopping", "Factorio is being stopped before the replacement setup Secret is acknowledged")
	if err := r.updateStatus(ctx, gameServer, status); err != nil {
		return true, ctrl.Result{}, err
	}
	return true, ctrl.Result{RequeueAfter: time.Second}, nil
}

func (r *GameServerReconciler) validateSetupSecret(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (factorio.Secrets, int64, error) {
	setup := gameServer.Spec.SelectedSetup
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: setup.Configuration.SecretRef.Name}
	if err := r.Get(ctx, key, secret); err != nil {
		return factorio.Secrets{}, 0, fmt.Errorf("read referenced setup Secret %q: %w", key.Name, err)
	}
	if secret.Labels[plexusv1alpha1.LabelServerID] != gameServer.Spec.ServerID ||
		secret.Labels[plexusv1alpha1.LabelOwnerUserID] != gameServer.Spec.OwnerUserID ||
		secret.Labels[plexusv1alpha1.LabelGameID] != setup.GameID || secret.Labels[plexusv1alpha1.LabelSetupID] != setup.ID {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q has different ownership", key.Name)
	}
	if secret.Annotations[factorio.SecretSchemaAnnotation] != factorio.SecretSchemaVersion {
		return factorio.Secrets{}, 0, &setupSecretMigrationError{name: key.Name, schemaVersion: secret.Annotations[factorio.SecretSchemaAnnotation]}
	}
	revision, err := strconv.ParseInt(secret.Annotations[factorio.SecretRevisionAnnotation], 10, 64)
	if err != nil || revision < 1 {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q has an invalid revision", key.Name)
	}
	if secret.Immutable == nil || *secret.Immutable == false {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q must be immutable", key.Name)
	}
	if secret.Type != corev1.SecretTypeOpaque {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q must use type Opaque", key.Name)
	}
	if len(secret.Data) != 1 {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q has unexpected data keys", key.Name)
	}
	secrets, err := factorio.DecodeSecrets(secret.Data[factorio.SecretDataKey])
	if err != nil {
		return factorio.Secrets{}, 0, fmt.Errorf("referenced setup Secret %q does not match the pinned adapter schema", key.Name)
	}
	return secrets, revision, nil
}

type setupSecretMigrationError struct {
	name          string
	schemaVersion string
}

func (err *setupSecretMigrationError) Error() string {
	return fmt.Sprintf("referenced setup Secret %q uses schema %q; publish a replacement using supported schema %q", err.name, err.schemaVersion, factorio.SecretSchemaVersion)
}

func (r *GameServerReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).
		For(&plexusv1alpha1.GameServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.PersistentVolumeClaim{}).
		Owns(&corev1.ConfigMap{}).
		Owns(&corev1.Secret{}).
		Complete(r)
}

func (r *GameServerReconciler) reconcileRunning(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, configuration factorio.Configuration, secrets factorio.Secrets, secretRevision int64) (ctrl.Result, error) {
	if _, err := r.ensureConfigMap(ctx, gameServer, configuration); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ConfigurationReconcileFailed", err)
	}
	if _, err := r.ensureRuntimeSecret(ctx, gameServer, secrets, secretRevision); err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ConfigurationReconcileFailed", err)
	}
	service, err := r.ensureService(ctx, gameServer, definition)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceReconcileFailed", err)
	}
	deployment, err := r.ensureDeployment(ctx, gameServer, definition, secretRevision)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadReconcileFailed", err)
	}

	endpoint, endpointReady := serviceEndpoint(service, definition.Ports[0])
	if deploymentRolloutAvailable(deployment) == false {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, "Waiting for the Factorio workload to become available")
		preserveActiveRevision(&status, gameServer.Status)
		status.Endpoint = endpoint
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadUnavailable", "Persistent storage and service are ready; the Factorio workload is not yet available")
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
		setEndpointCondition(&status, gameServer.Generation, endpointReady)
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
	}
	if !endpointReady {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStarting, "Factorio is running; waiting for a public service endpoint")
		acknowledgeActiveRevision(&status, gameServer, secretRevision)
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "EndpointPending", "Factorio is available, but the load balancer has not assigned a public endpoint")
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
		setEndpointCondition(&status, gameServer.Generation, false)
		return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
	}

	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseRunning, "Factorio workload is running")
	acknowledgeActiveRevision(&status, gameServer, secretRevision)
	status.Endpoint = endpoint
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionTrue, "WorkloadAvailable", "Factorio workload is available")
	setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is ready")
	setEndpointCondition(&status, gameServer.Generation, true)
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

func (r *GameServerReconciler) ensureRuntimeSecret(ctx context.Context, gameServer *plexusv1alpha1.GameServer, secrets factorio.Secrets, revision int64) (*corev1.Secret, error) {
	name := runtimeSecretName(gameServer, revision)
	secret := &corev1.Secret{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: name}
	desiredData := map[string][]byte{
		"USERNAME":      []byte(secrets.Username),
		"TOKEN":         []byte(secrets.Token),
		"GAME_PASSWORD": []byte(secrets.GamePassword),
		"RCON_PASSWORD": []byte(secrets.RCONPassword),
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
			Annotations: map[string]string{factorio.SecretRevisionAnnotation: strconv.FormatInt(revision, 10)},
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

func (r *GameServerReconciler) ensureConfigMap(ctx context.Context, gameServer *plexusv1alpha1.GameServer, configuration factorio.Configuration) (*corev1.ConfigMap, error) {
	settings, err := renderFactorioSettings(configuration)
	if err != nil {
		return nil, err
	}
	name := runtimeConfigMapName(gameServer)
	configMap := &corev1.ConfigMap{}
	key := client.ObjectKey{Namespace: gameServer.Namespace, Name: name}
	desiredData := map[string]string{configFileName: settings}
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

func (r *GameServerReconciler) reconcileStopped(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (ctrl.Result, error) {
	deleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	serviceDeleted, err := r.deleteService(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceCleanupFailed", err)
	}
	if deleted || serviceDeleted {
		status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopping, "Stopping the Factorio workload; persistent storage is retained")
		setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "WorkloadStopping", "Factorio workload is being removed")
		setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is retained")
		setCondition(&status, gameServer.Generation, conditionEndpoint, metav1.ConditionFalse, "ServiceStopping", "Public endpoint is being removed")
		if err := r.updateStatus(ctx, gameServer, status); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: time.Second}, nil
	}

	status := observedStatus(gameServer, plexusv1alpha1.GameServerPhaseStopped, "Factorio workload is stopped; persistent storage is retained")
	setCondition(&status, gameServer.Generation, conditionReady, metav1.ConditionFalse, "DesiredStopped", "No Factorio workload is running")
	setCondition(&status, gameServer.Generation, conditionStorage, metav1.ConditionTrue, "PersistentVolumeReady", "Persistent game storage is retained")
	setCondition(&status, gameServer.Generation, conditionEndpoint, metav1.ConditionFalse, "DesiredStopped", "A stopped server has no public endpoint")
	return ctrl.Result{RequeueAfter: observationRefreshInterval}, r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) reconcileUnloaded(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (ctrl.Result, error) {
	deleted, err := r.deleteDeployment(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "WorkloadStopFailed", err)
	}
	serviceDeleted, err := r.deleteService(ctx, gameServer)
	if err != nil {
		return ctrl.Result{}, r.reportFailure(ctx, gameServer, "ServiceCleanupFailed", err)
	}
	if deleted || serviceDeleted {
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

func (r *GameServerReconciler) ensureDeployment(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, secretRevision int64) (*appsv1.Deployment, error) {
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	_, err := controllerutil.CreateOrUpdate(ctx, r.Client, deployment, func() error {
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
					"plexus.gg/restart-generation":       fmt.Sprint(gameServer.Spec.RestartGeneration),
					"plexus.gg/configuration-generation": fmt.Sprint(gameServer.Generation),
					factorio.SecretRevisionAnnotation:    strconv.FormatInt(secretRevision, 10),
				},
			},
			Spec: corev1.PodSpec{
				InitContainers: []corev1.Container{{
					Name:    "factorio-config-init",
					Image:   definition.DefaultImage,
					Command: []string{"/bin/sh", "-c"},
					Args:    []string{"cp /plexus/config/server-settings.json /factorio/config/server-settings.json"},
					VolumeMounts: []corev1.VolumeMount{
						{Name: configSourceName, MountPath: configSourcePath, ReadOnly: true},
						{Name: configVolumeName, MountPath: configMountPath},
					},
				}},
				Containers: []corev1.Container{{
					Name:  factorio.GameID,
					Image: definition.DefaultImage,
					Env:   environment(definition, runtimeSecretName(gameServer, secretRevision)),
					Ports: containerPorts(definition),
					VolumeMounts: []corev1.VolumeMount{
						{Name: dataVolumeName, MountPath: dataMountPath},
						{Name: configVolumeName, MountPath: configMountPath},
					},
				}},
				Volumes: []corev1.Volume{
					{Name: dataVolumeName, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: gameServer.Name}}},
					{Name: configSourceName, VolumeSource: corev1.VolumeSource{ConfigMap: &corev1.ConfigMapVolumeSource{LocalObjectReference: corev1.LocalObjectReference{Name: runtimeConfigMapName(gameServer)}}}},
					{Name: configVolumeName, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
				},
			},
		}
		return nil
	})
	return deployment, err
}

func (r *GameServerReconciler) reconcileDelete(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (ctrl.Result, error) {
	if !controllerutil.ContainsFinalizer(gameServer, GameServerFinalizer) {
		return ctrl.Result{}, nil
	}

	remaining := false
	for _, object := range []client.Object{
		&appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}},
		&corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}},
		&corev1.PersistentVolumeClaim{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}},
	} {
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

func renderFactorioSettings(configuration factorio.Configuration) (string, error) {
	tags := configuration.Tags
	if tags == nil {
		tags = []string{}
	}
	settings := struct {
		Name                    string              `json:"name"`
		Description             string              `json:"description"`
		Tags                    []string            `json:"tags"`
		MaxPlayers              int                 `json:"max_players"`
		Visibility              factorio.Visibility `json:"visibility"`
		RequireUserVerification bool                `json:"require_user_verification"`
		AllowCommands           string              `json:"allow_commands"`
		AutosaveInterval        int                 `json:"autosave_interval"`
		AutosaveSlots           int                 `json:"autosave_slots"`
		AFKAutokickInterval     int                 `json:"afk_autokick_interval"`
		AutoPause               bool                `json:"auto_pause"`
		OnlyAdminsCanPause      bool                `json:"only_admins_can_pause_the_game"`
		AutosaveOnlyOnServer    bool                `json:"autosave_only_on_server"`
		NonBlockingSaving       bool                `json:"non_blocking_saving"`
	}{
		Name: configuration.Name, Description: configuration.Description, Tags: tags,
		MaxPlayers: configuration.MaxPlayers, Visibility: configuration.Visibility,
		RequireUserVerification: configuration.RequireUserVerification, AllowCommands: configuration.AllowCommands,
		AutosaveInterval: configuration.Autosave.IntervalMinutes, AutosaveSlots: configuration.Autosave.Slots,
		AFKAutokickInterval: configuration.AFKAutokickMinutes, AutoPause: configuration.AutoPause,
		OnlyAdminsCanPause: configuration.OnlyAdminsCanPause, AutosaveOnlyOnServer: configuration.AutosaveOnlyOnServer,
		NonBlockingSaving: configuration.NonBlockingSaving,
	}
	rendered, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return "", fmt.Errorf("render Factorio server settings: %w", err)
	}
	return string(append(rendered, '\n')), nil
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
	deployment := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{Name: gameServer.Name, Namespace: gameServer.Namespace}}
	if err := r.Get(ctx, client.ObjectKeyFromObject(deployment), deployment); err != nil {
		return false, client.IgnoreNotFound(err)
	}
	if err := ensureControlledBy(gameServer, deployment); err != nil {
		return false, err
	}
	if !deployment.DeletionTimestamp.IsZero() {
		return true, nil
	}
	return true, r.Delete(ctx, deployment)
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

func setCondition(status *plexusv1alpha1.GameServerStatus, generation int64, conditionType string, conditionStatus metav1.ConditionStatus, reason string, message string) {
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type: conditionType, Status: conditionStatus, ObservedGeneration: generation,
		Reason: reason, Message: message,
	})
}

func setEndpointCondition(status *plexusv1alpha1.GameServerStatus, generation int64, ready bool) {
	if ready {
		setCondition(status, generation, conditionEndpoint, metav1.ConditionTrue, "LoadBalancerReady", "Public Factorio endpoint is assigned")
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
	if gameServer.Spec.SelectedSetup.GameID != factorio.GameID {
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
	labels[plexusv1alpha1.LabelOwnerUserID] = gameServer.Spec.OwnerUserID
	labels[plexusv1alpha1.LabelGameID] = gameServer.Spec.SelectedSetup.GameID
	labels[plexusv1alpha1.LabelSetupID] = gameServer.Spec.SelectedSetup.ID
	return labels
}

func selectorLabels(gameServer *plexusv1alpha1.GameServer) map[string]string {
	return map[string]string{
		plexusv1alpha1.LabelServerID: gameServer.Spec.ServerID,
		componentLabel:               componentValue,
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

func environment(definition games.GameDefinition, runtimeSecretName string) []corev1.EnvVar {
	names := make([]string, 0, len(definition.DefaultEnv))
	for name := range definition.DefaultEnv {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]corev1.EnvVar, 0, len(names))
	for _, name := range names {
		environment = append(environment, corev1.EnvVar{Name: name, Value: definition.DefaultEnv[name]})
	}
	for _, name := range []string{"GAME_PASSWORD", "RCON_PASSWORD", "TOKEN", "USERNAME"} {
		environment = append(environment, corev1.EnvVar{
			Name: name,
			ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{
				LocalObjectReference: corev1.LocalObjectReference{Name: runtimeSecretName}, Key: name,
			}},
		})
	}
	return environment
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

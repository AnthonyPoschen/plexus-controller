package controller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	plexusv1alpha1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/internal/games"
)

const (
	diskJobContainer          = "managed-disk"
	diskWorkAnnotation        = "plexus.gg/disk-work"
	diskFingerprintAnnotation = "plexus.gg/disk-fingerprint"
	diskWorkMods              = "mods"
	pendingWorkApplyingMods   = "Applying %s mods"
	pendingWorkApplyingSave   = "Applying save data"
	pendingWorkExportingSave  = "Exporting save data"
	modInstallFailedMessage   = "%s mod synchronization failed"
)

type diskReconcile struct {
	blocked bool
	failed  bool
	reason  string
	message string
}

func (r *GameServerReconciler) reconcileManagedDisk(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) (diskReconcile, error) {
	if work, err := r.pendingSaveDiskWork(ctx, gameServer); err != nil {
		return diskReconcile{}, err
	} else if work.blocked || work.failed {
		return work, nil
	}

	if definition.Workload.SupportsMods == false {
		return diskReconcile{}, nil
	}
	desired := observedInstalledMods(gameServer.Spec.SelectedSetup.Mods)
	if installedModsMatch(gameServer.Status.InstalledMods, desired) {
		if err := r.deleteCompletedDiskJob(ctx, gameServer); err != nil {
			return diskReconcile{}, err
		}
		return diskReconcile{}, nil
	}

	fingerprint := desiredModFingerprint(desired)
	job, err := r.ownedDiskJob(ctx, gameServer)
	if err != nil {
		return diskReconcile{}, err
	}
	if job == nil {
		if err := r.createModDiskJob(ctx, gameServer, definition, fingerprint); err != nil {
			return diskReconcile{}, err
		}
		return diskReconcile{blocked: true, reason: "DiskJobRunning", message: fmt.Sprintf(pendingWorkApplyingMods, definition.DisplayName)}, nil
	}
	if job.Annotations[diskFingerprintAnnotation] != fingerprint {
		if diskJobFinished(job) {
			if err := r.deleteOwnedObjectStrict(ctx, gameServer, job); err != nil {
				return diskReconcile{}, err
			}
			if err := r.createModDiskJob(ctx, gameServer, definition, fingerprint); err != nil {
				return diskReconcile{}, err
			}
		}
		return diskReconcile{blocked: true, reason: "DiskJobRunning", message: fmt.Sprintf(pendingWorkApplyingMods, definition.DisplayName)}, nil
	}
	if diskJobFailed(job) {
		return diskReconcile{blocked: true, failed: true, reason: "ModInstallFailed", message: fmt.Sprintf(modInstallFailedMessage, definition.DisplayName)}, nil
	}
	if diskJobSucceeded(job) {
		if err := r.acknowledgeDiskJobMods(ctx, gameServer, desired); err != nil {
			return diskReconcile{}, err
		}
		if err := r.deleteOwnedObjectStrict(ctx, gameServer, job); err != nil {
			return diskReconcile{}, err
		}
		return diskReconcile{}, nil
	}
	return diskReconcile{blocked: true, reason: "DiskJobRunning", message: fmt.Sprintf(pendingWorkApplyingMods, definition.DisplayName)}, nil
}

func (r *GameServerReconciler) pendingSaveDiskWork(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (diskReconcile, error) {
	var imports plexusv1alpha1.SaveImportList
	if err := r.List(ctx, &imports, client.InNamespace(gameServer.Namespace)); err != nil {
		return diskReconcile{}, err
	}
	for _, replacement := range imports.Items {
		if replacement.Spec.ServerID != gameServer.Spec.ServerID {
			continue
		}
		if replacement.Status.Phase == plexusv1alpha1.SaveImportFailed {
			return diskReconcile{
				blocked: true,
				failed:  gameServer.Spec.DesiredPower == plexusv1alpha1.DesiredPowerRunning,
				reason:  "SaveImportFailed",
				message: "Save data replacement failed",
			}, nil
		}
		if saveImportHoldsPVC(replacement) {
			return diskReconcile{blocked: true, reason: "DiskJobRunning", message: pendingWorkApplyingSave}, nil
		}
	}

	var exports plexusv1alpha1.SaveExportList
	if err := r.List(ctx, &exports, client.InNamespace(gameServer.Namespace)); err != nil {
		return diskReconcile{}, err
	}
	for _, export := range exports.Items {
		if export.Spec.ServerID != gameServer.Spec.ServerID {
			continue
		}
		if saveExportHoldsPVC(export) {
			return diskReconcile{blocked: true, reason: "DiskJobRunning", message: pendingWorkExportingSave}, nil
		}
	}
	return diskReconcile{}, nil
}

func saveImportHoldsPVC(replacement plexusv1alpha1.SaveImport) bool {
	switch replacement.Status.Phase {
	case "", plexusv1alpha1.SaveImportPending, plexusv1alpha1.SaveImportRunning:
		return true
	default:
		return false
	}
}

func saveExportHoldsPVC(export plexusv1alpha1.SaveExport) bool {
	switch export.Status.Phase {
	case "", plexusv1alpha1.SaveExportPending, plexusv1alpha1.SaveExportRunning:
		return true
	default:
		return false
	}
}

func (r *GameServerReconciler) ownedDiskJob(ctx context.Context, gameServer *plexusv1alpha1.GameServer) (*batchv1.Job, error) {
	job := &batchv1.Job{}
	err := r.Get(ctx, client.ObjectKey{Namespace: gameServer.Namespace, Name: diskJobName(gameServer)}, job)
	if apierrors.IsNotFound(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if err := ensureControlledBy(gameServer, job); err != nil {
		return nil, err
	}
	return job, nil
}

func (r *GameServerReconciler) createModDiskJob(ctx context.Context, gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, fingerprint string) error {
	job := r.modDiskJobFor(gameServer, definition, fingerprint)
	if err := controllerutil.SetControllerReference(gameServer, job, r.Scheme); err != nil {
		return err
	}
	return r.Create(ctx, job)
}

func (r *GameServerReconciler) acknowledgeDiskJobMods(ctx context.Context, gameServer *plexusv1alpha1.GameServer, installed []plexusv1alpha1.InstalledMod) error {
	status := gameServer.Status
	status.Conditions = append([]metav1.Condition(nil), gameServer.Status.Conditions...)
	status.InstalledMods = installed
	status.InstalledModsGeneration = gameServer.Generation
	return r.updateStatus(ctx, gameServer, status)
}

func (r *GameServerReconciler) deleteCompletedDiskJob(ctx context.Context, gameServer *plexusv1alpha1.GameServer) error {
	job, err := r.ownedDiskJob(ctx, gameServer)
	if err != nil || job == nil {
		return err
	}
	if diskJobFinished(job) == false {
		return nil
	}
	return r.deleteOwnedObjectStrict(ctx, gameServer, job)
}

func (r *GameServerReconciler) deleteOwnedObjectStrict(ctx context.Context, gameServer *plexusv1alpha1.GameServer, object client.Object) error {
	if err := ensureControlledBy(gameServer, object); err != nil {
		return err
	}
	if object.GetDeletionTimestamp() != nil {
		return nil
	}
	return client.IgnoreNotFound(r.Delete(ctx, object, client.PropagationPolicy(metav1.DeletePropagationBackground)))
}

func (r *GameServerReconciler) modDiskJobFor(gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition, fingerprint string) *batchv1.Job {
	zero := int32(0)
	labels := diskJobLabels(gameServer)
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name:        diskJobName(gameServer),
		Namespace:   gameServer.Namespace,
		Labels:      labels,
		Annotations: map[string]string{diskWorkAnnotation: diskWorkMods, diskFingerprintAnnotation: fingerprint},
	}}
	job.Spec.BackoffLimit = &zero
	job.Spec.Template.ObjectMeta.Labels = labels
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.Containers = []corev1.Container{factorioModSyncContainer(gameServer, definition)}
	job.Spec.Template.Spec.Volumes = append([]corev1.Volume{{
		Name:         dataVolumeName,
		VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: gameServer.Name}},
	}}, modArtifactVolumes(gameServer, definition)...)
	return job
}

func factorioModSyncContainer(gameServer *plexusv1alpha1.GameServer, definition games.GameDefinition) corev1.Container {
	command := "mkdir -p /factorio/mods && find /factorio/mods -maxdepth 1 -type f -name '*.zip' -delete"
	mounts := []corev1.VolumeMount{{Name: dataVolumeName, MountPath: definition.Workload.DataMountPath}}
	if len(gameServer.Spec.SelectedSetup.Mods) == 1 {
		mod := gameServer.Spec.SelectedSetup.Mods[0]
		command += " && cp /plexus/mod/archive.zip /factorio/mods/" + mod.ArchiveFileName
		mounts = append(mounts, corev1.VolumeMount{Name: modSourceName, MountPath: modSourcePath, ReadOnly: true})
	}
	return corev1.Container{Name: diskJobContainer, Image: definition.DefaultImage, Command: []string{"/bin/sh", "-eu", "-c"}, Args: []string{command}, VolumeMounts: mounts}
}

func diskJobName(gameServer *plexusv1alpha1.GameServer) string {
	return revisionScopedResourceName(gameServer.Name, "-disk")
}

func diskJobLabels(gameServer *plexusv1alpha1.GameServer) map[string]string {
	return map[string]string{
		plexusv1alpha1.LabelServerID:      gameServer.Spec.ServerID,
		plexusv1alpha1.LabelGameServerUID: string(gameServer.UID),
		plexusv1alpha1.LabelOwnerUserID:   gameServer.Spec.OwnerUserID,
		plexusv1alpha1.LabelGameID:        gameServer.Spec.SelectedSetup.GameID,
		plexusv1alpha1.LabelSetupID:       gameServer.Spec.SelectedSetup.ID,
		plexusv1alpha1.LabelComponent:     plexusv1alpha1.ComponentManagedDisk,
	}
}

func desiredModFingerprint(mods []plexusv1alpha1.InstalledMod) string {
	payload, err := json.Marshal(mods)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256(payload)
	return hex.EncodeToString(sum[:])
}

func installedModsMatch(got []plexusv1alpha1.InstalledMod, want []plexusv1alpha1.InstalledMod) bool {
	if len(got) != len(want) {
		return false
	}
	for index := range want {
		if got[index].ProviderID != want[index].ProviderID || got[index].ProviderModID != want[index].ProviderModID ||
			got[index].Name != want[index].Name || got[index].Version != want[index].Version {
			return false
		}
	}
	return true
}

func diskJobSucceeded(job *batchv1.Job) bool {
	return job.Status.Succeeded > 0
}

func diskJobFailed(job *batchv1.Job) bool {
	if job.Status.Failed > 0 {
		return true
	}
	for _, condition := range job.Status.Conditions {
		if condition.Type == batchv1.JobFailed && condition.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

func diskJobFinished(job *batchv1.Job) bool {
	return diskJobSucceeded(job) || diskJobFailed(job)
}

func enqueueGameServerForServerID(object client.Object) []reconcile.Request {
	serverID := object.GetLabels()[plexusv1alpha1.LabelServerID]
	switch typed := object.(type) {
	case *plexusv1alpha1.SaveImport:
		if typed.Spec.ServerID != "" {
			serverID = typed.Spec.ServerID
		}
	case *plexusv1alpha1.SaveExport:
		if typed.Spec.ServerID != "" {
			serverID = typed.Spec.ServerID
		}
	}
	if serverID == "" {
		return nil
	}
	return []reconcile.Request{{NamespacedName: types.NamespacedName{Namespace: object.GetNamespace(), Name: serverID}}}
}

func mapServerOwnedRequest(ctx context.Context, object client.Object) []reconcile.Request {
	return enqueueGameServerForServerID(object)
}

var _ handler.MapFunc = mapServerOwnedRequest

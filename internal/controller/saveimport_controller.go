package controller

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	plexusv1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
	"github.com/AnthonyPoschen/plexus-controller/internal/games"
)

const (
	SaveImportDownloadURLKey = "download-url"
	LabelSaveImportID        = "plexus.gg/save-import-id"
	SaveImportFinalizer      = "plexus.gg/save-import-cleanup"
	saveImporterContainer    = "save-importer"
	saveImportTargetVolume   = "save-target"
	saveImportWorkVolume     = "save-work"
	saveImportTargetPath     = "/target"
	saveImportWorkPath       = "/work"
	saveImportFreshFor       = 2 * time.Minute
)

type SaveImportReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ImporterImage string
	Progress      SaveExportProgressReader
	Now           func() time.Time
}

// +kubebuilder:rbac:groups=plexus.gg,resources=saveimports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=saveimports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=saveimports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

func (r *SaveImportReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var replacement plexusv1.SaveImport
	if err := r.Get(ctx, request.NamespacedName, &replacement); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if !replacement.DeletionTimestamp.IsZero() {
		diagnostics, err := r.cleanup(ctx, &replacement, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(diagnostics) != 0 {
			replacement.Status.Phase = plexusv1.SaveImportFailed
			replacement.Status.Stage = "cleanup"
			replacement.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
			if err := r.Status().Update(ctx, &replacement); err != nil {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(&replacement, SaveImportFinalizer) {
			return ctrl.Result{}, r.Update(ctx, &replacement)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&replacement, SaveImportFinalizer) {
		if err := r.Update(ctx, &replacement); err != nil {
			return ctrl.Result{}, err
		}
	}
	if replacement.Status.Phase == plexusv1.SaveImportSucceeded || replacement.Status.Phase == plexusv1.SaveImportFailed || replacement.Status.Phase == plexusv1.SaveImportExpired {
		removeJob := !replacement.Spec.ExpiresAt.After(now)
		diagnostics, err := r.cleanup(ctx, &replacement, removeJob)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(diagnostics) != 0 && replacement.Status.Message != boundedDiagnostic(strings.Join(diagnostics, "; ")) {
			replacement.Status.Stage = "cleanup"
			replacement.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
			if err := r.Status().Update(ctx, &replacement); err != nil {
				return ctrl.Result{}, err
			}
		}
		if removeJob {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: max(time.Second, replacement.Spec.ExpiresAt.Sub(now))}, nil
	}
	if !replacement.Spec.ExpiresAt.After(now) {
		return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportExpired, "expired", "none", "The save replacement authorization expired before completion")
	}
	definition, err := r.authorize(ctx, &replacement, now)
	if err != nil {
		return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportFailed, "authorization", "none", err.Error())
	}

	var job batchv1.Job
	err = r.Get(ctx, request.NamespacedName, &job)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, r.jobFor(&replacement, definition, now)); err != nil {
			return ctrl.Result{}, err
		}
		startedAt := metav1.NewTime(now)
		replacement.Status = plexusv1.SaveImportStatus{Phase: plexusv1.SaveImportRunning, ProgressPercent: 0, Stage: "starting", ArchiveName: replacement.Spec.ArchiveName, StartedAt: &startedAt, Message: "The importer Job is starting"}
		if err := r.Status().Update(ctx, &replacement); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if metav1.IsControlledBy(&job, &replacement) == false {
		return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportFailed, "cleanup", "none", "A same-name Job has different ownership; it was left untouched")
	}
	if job.Status.Succeeded > 0 {
		result, found := r.jobTerminationResult(ctx, &job)
		if found == false || result.Stage != "complete" || result.ArchiveBytes <= 0 {
			return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportFailed, "observation", "none", "The importer completed without valid archive size metadata. Recovery status is unknown.")
		}
		replacement.Status.ArchiveBytes = result.ArchiveBytes
		recovery := result.Recovery
		if recovery == "" {
			recovery = "snapshot-created"
		}
		return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportSucceeded, "complete", recovery, "The hosted save was replaced. A recovery snapshot of the previous save is retained. The Server remains stopped.")
	}
	if job.Status.Failed > 0 {
		result, found := r.jobTerminationResult(ctx, &job)
		if found == false || validImportFailureStage(result.Stage) == false {
			return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportFailed, "job", "none", "The hosted save replacement failed before a recovery outcome was recorded.")
		}
		return ctrl.Result{}, r.finish(ctx, &replacement, plexusv1.SaveImportFailed, result.Stage, importFailureRecovery(result), boundedDiagnostic(importFailureMessage(result)))
	}
	if replacement.Status.Phase != plexusv1.SaveImportRunning {
		startedAt := metav1.NewTime(now)
		replacement.Status = plexusv1.SaveImportStatus{Phase: plexusv1.SaveImportRunning, ProgressPercent: 0, Stage: "starting", ArchiveName: replacement.Spec.ArchiveName, StartedAt: &startedAt, Message: "The importer Job is starting"}
		if err := r.Status().Update(ctx, &replacement); err != nil {
			return ctrl.Result{}, err
		}
	}
	if updated, err := r.observeProgress(ctx, &replacement, &job); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to observe bounded save import progress", "saveImport", replacement.Name)
	} else if updated {
		if err := r.Status().Update(ctx, &replacement); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: min(5*time.Second, replacement.Spec.ExpiresAt.Sub(now))}, nil
}

func (r *SaveImportReconciler) observeProgress(ctx context.Context, replacement *plexusv1.SaveImport, job *batchv1.Job) (bool, error) {
	if r.Progress == nil {
		return false, nil
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return false, err
	}
	latest := exporterProgress{}
	found := false
	for index := range pods.Items {
		pod := &pods.Items[index]
		if metav1.IsControlledBy(pod, job) == false {
			continue
		}
		progress, present, err := r.Progress.Latest(ctx, pod.Namespace, pod.Name)
		if err != nil {
			return false, err
		}
		if present && (found == false || progress.ProgressPercent > latest.ProgressPercent) {
			latest, found = progress, true
		}
	}
	if found == false || latest.ProgressPercent <= replacement.Status.ProgressPercent {
		return false, nil
	}
	replacement.Status.Phase = plexusv1.SaveImportRunning
	replacement.Status.ProgressPercent = latest.ProgressPercent
	replacement.Status.Stage = latest.Stage
	replacement.Status.Message = importProgressMessage(latest)
	return true, nil
}

func importProgressMessage(progress exporterProgress) string {
	switch progress.Stage {
	case "download":
		if progress.ProgressPercent < 25 {
			return "Downloading the uploaded save archive"
		}
		return "The uploaded save archive was downloaded"
	case "validation":
		return "Validating the uploaded save archive"
	case "snapshot":
		if progress.ProgressPercent < 60 {
			return "Creating an automatic recovery snapshot of the hosted save"
		}
		return "The automatic recovery snapshot is ready"
	case "replace":
		if progress.ProgressPercent < 85 {
			return "Replacing the hosted save archive"
		}
		return "The hosted save replacement is finalizing"
	case "rollback":
		return "Restoring the previous hosted save from the recovery snapshot"
	default:
		return "The save replacement is running"
	}
}

func importFailureRecovery(result exporterTermination) string {
	switch result.Recovery {
	case "snapshot-created", "restored", "rollback-failed", "none":
		return result.Recovery
	}
	switch result.Stage {
	case "replace":
		return "restored"
	case "rollback":
		return "rollback-failed"
	default:
		return "none"
	}
}

func importFailureMessage(result exporterTermination) string {
	if importDiagnosticMentionsRecovery(result.Message) {
		return fmt.Sprintf("Save replacement failed during %s: %s", result.Stage, result.Message)
	}
	switch result.Recovery {
	case "restored":
		return fmt.Sprintf("Save replacement failed during %s: %s The previous hosted save was restored from the automatic recovery snapshot.", result.Stage, result.Message)
	case "rollback-failed":
		return fmt.Sprintf("Save replacement failed during %s: %s Automatic rollback failed; a recoverable safety snapshot is retained.", result.Stage, result.Message)
	case "snapshot-created":
		return fmt.Sprintf("Save replacement failed during %s: %s A recovery snapshot of the previous save is retained.", result.Stage, result.Message)
	}
	switch result.Stage {
	case "snapshot":
		return fmt.Sprintf("Save replacement failed during snapshot: %s The hosted save was not replaced.", result.Message)
	case "replace":
		return fmt.Sprintf("Save replacement failed during replace: %s The previous hosted save was restored from the automatic recovery snapshot.", result.Message)
	case "rollback":
		return fmt.Sprintf("Save replacement failed during rollback: %s Automatic rollback failed; a recoverable safety snapshot is retained.", result.Message)
	default:
		return fmt.Sprintf("Save replacement failed during %s: %s Plexus did not create an automatic recovery snapshot.", result.Stage, result.Message)
	}
}

func importDiagnosticMentionsRecovery(message string) bool {
	lower := strings.ToLower(message)
	return strings.Contains(lower, "snapshot") || strings.Contains(lower, "restored") || strings.Contains(lower, "rollback")
}

func (r *SaveImportReconciler) authorize(ctx context.Context, replacement *plexusv1.SaveImport, now time.Time) (games.GameDefinition, error) {
	var gameServer plexusv1.GameServer
	if err := r.Get(ctx, client.ObjectKey{Namespace: replacement.Namespace, Name: replacement.Spec.ServerID}, &gameServer); err != nil {
		return games.GameDefinition{}, fmt.Errorf("owned server runtime was not found")
	}
	if !gameServer.DeletionTimestamp.IsZero() {
		return games.GameDefinition{}, fmt.Errorf("owned server runtime is being deleted")
	}
	if gameServer.Spec.ServerID != replacement.Spec.ServerID || gameServer.Spec.OwnerUserID != replacement.Spec.OwnerUserID || gameServer.Spec.SelectedSetup == nil ||
		gameServer.Spec.SelectedSetup.ID != replacement.Spec.SetupID || gameServer.Spec.SelectedSetup.GameID != replacement.Spec.GameID {
		return games.GameDefinition{}, fmt.Errorf("server ownership or selected setup changed")
	}
	if gameServer.Spec.DesiredPower != plexusv1.DesiredPowerStopped || gameServer.Status.Phase != plexusv1.GameServerPhaseStopped ||
		gameServer.Status.ObservedGeneration != gameServer.Generation || gameServer.Status.LastObservedAt == nil || importObservationFresh(now, gameServer.Status.LastObservedAt.Time) == false {
		return games.GameDefinition{}, fmt.Errorf("server is not freshly confirmed stopped")
	}
	definition, err := games.Get(replacement.Spec.GameID)
	if err != nil || definition.SaveImport == nil {
		return games.GameDefinition{}, fmt.Errorf("selected game does not support save replacement")
	}
	if r.ImporterImage == "" {
		return games.GameDefinition{}, fmt.Errorf("save importer image is not configured")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: replacement.Namespace, Name: replacement.Spec.DownloadURLSecretRef}, &secret); err != nil {
		return games.GameDefinition{}, fmt.Errorf("save replacement transfer authorization is unavailable")
	}
	labels := secret.Labels
	if labels[plexusv1.LabelServerID] != replacement.Spec.ServerID || labels[plexusv1.LabelOwnerUserID] != replacement.Spec.OwnerUserID ||
		labels[plexusv1.LabelSetupID] != replacement.Spec.SetupID || labels[plexusv1.LabelGameID] != replacement.Spec.GameID || labels[LabelSaveImportID] != replacement.Name ||
		secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 || len(secret.Data[SaveImportDownloadURLKey]) == 0 {
		return games.GameDefinition{}, fmt.Errorf("save replacement transfer authorization has different ownership or invalid content")
	}
	return definition, nil
}

func (r *SaveImportReconciler) jobFor(replacement *plexusv1.SaveImport, definition games.GameDefinition, now time.Time) *batchv1.Job {
	zero := int32(0)
	deadline := int64(replacement.Spec.ExpiresAt.Sub(now).Seconds())
	runAsNonRoot := true
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: replacement.Name, Namespace: replacement.Namespace, Labels: importLabels(replacement)}}
	_ = controllerutil.SetControllerReference(replacement, job, r.Scheme)
	job.Spec.BackoffLimit = &zero
	job.Spec.ActiveDeadlineSeconds = &deadline
	job.Spec.Template.ObjectMeta.Labels = importLabels(replacement)
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.AutomountServiceAccountToken = boolPointer(false)
	job.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: saveImporterContainer, Image: r.ImporterImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "PLEXUS_DOWNLOAD_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: replacement.Spec.DownloadURLSecretRef}, Key: SaveImportDownloadURLKey}}},
			{Name: "PLEXUS_SAVE_TARGET_LAYOUT", Value: string(definition.SaveImport.TargetLayout)},
			{Name: "PLEXUS_SAVE_REPLACEMENT", Value: string(definition.SaveImport.Replacement)},
			{Name: "PLEXUS_ARCHIVE_NAME", Value: replacement.Spec.ArchiveName},
			{Name: "PLEXUS_SAVE_IMPORT_ID", Value: replacement.Name},
		},
		SecurityContext: &corev1.SecurityContext{RunAsNonRoot: &runAsNonRoot, ReadOnlyRootFilesystem: &readOnlyRoot, AllowPrivilegeEscalation: &allowPrivilegeEscalation, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
		VolumeMounts: []corev1.VolumeMount{
			{Name: saveImportTargetVolume, MountPath: saveImportTargetPath, SubPath: definition.SaveImport.PVCSubPath},
			{Name: saveImportWorkVolume, MountPath: saveImportWorkPath},
		},
	}}
	job.Spec.Template.Spec.Volumes = []corev1.Volume{
		{Name: saveImportTargetVolume, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: replacement.Spec.ServerID}}},
		{Name: saveImportWorkVolume, VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}}},
	}
	return job
}

func (r *SaveImportReconciler) finish(ctx context.Context, replacement *plexusv1.SaveImport, phase plexusv1.SaveImportPhase, stage string, recovery string, message string) error {
	finishedAt := metav1.NewTime(time.Now().UTC())
	if r.Now != nil {
		finishedAt = metav1.NewTime(r.Now().UTC())
	}
	replacement.Status.Phase = phase
	replacement.Status.Stage = stage
	replacement.Status.Recovery = recovery
	replacement.Status.Message = boundedDiagnostic(message)
	replacement.Status.FinishedAt = &finishedAt
	if phase == plexusv1.SaveImportSucceeded {
		replacement.Status.ProgressPercent = 100
	}
	if err := r.Status().Update(ctx, replacement); err != nil {
		return err
	}
	diagnostics, err := r.cleanup(ctx, replacement, phase == plexusv1.SaveImportExpired)
	if err != nil {
		return err
	}
	if len(diagnostics) == 0 {
		return nil
	}
	replacement.Status.Stage = "cleanup"
	replacement.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
	return r.Status().Update(ctx, replacement)
}

func (r *SaveImportReconciler) cleanup(ctx context.Context, replacement *plexusv1.SaveImport, removeJob bool) ([]string, error) {
	diagnostics := []string{}
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: replacement.Namespace, Name: replacement.Name}, &job)
	if err == nil && metav1.IsControlledBy(&job, replacement) == false {
		diagnostics = append(diagnostics, "A same-name Job has different ownership and was left untouched")
	}
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if removeJob && err == nil && metav1.IsControlledBy(&job, replacement) {
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
	}
	var secret corev1.Secret
	err = r.Get(ctx, client.ObjectKey{Namespace: replacement.Namespace, Name: replacement.Spec.DownloadURLSecretRef}, &secret)
	if err == nil && saveImportAuthorizationOwnedBy(&secret, replacement) == false {
		diagnostics = append(diagnostics, "The referenced Secret has different ownership and was left untouched")
	}
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && saveImportAuthorizationOwnedBy(&secret, replacement) {
		if err := r.Delete(ctx, &secret); err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
	}
	return diagnostics, nil
}

func (r *SaveImportReconciler) jobTerminationResult(ctx context.Context, job *batchv1.Job) (exporterTermination, bool) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(job.Namespace), client.MatchingLabels{"job-name": job.Name}); err != nil {
		return exporterTermination{}, false
	}
	for index := range pods.Items {
		pod := &pods.Items[index]
		if metav1.IsControlledBy(pod, job) == false {
			continue
		}
		for _, status := range pod.Status.ContainerStatuses {
			if status.Name != saveImporterContainer || status.State.Terminated == nil {
				continue
			}
			var result exporterTermination
			if json.Unmarshal([]byte(status.State.Terminated.Message), &result) != nil {
				return exporterTermination{}, false
			}
			result.Message = boundedDiagnostic(result.Message)
			return result, true
		}
	}
	return exporterTermination{}, false
}

func saveImportAuthorizationOwnedBy(secret *corev1.Secret, replacement *plexusv1.SaveImport) bool {
	labels := secret.Labels
	return labels[plexusv1.LabelServerID] == replacement.Spec.ServerID && labels[plexusv1.LabelOwnerUserID] == replacement.Spec.OwnerUserID && labels[plexusv1.LabelSetupID] == replacement.Spec.SetupID && labels[plexusv1.LabelGameID] == replacement.Spec.GameID && labels[LabelSaveImportID] == replacement.Name
}

func importObservationFresh(now time.Time, observed time.Time) bool {
	age := now.Sub(observed)
	return age >= -30*time.Second && age <= saveImportFreshFor
}

func importLabels(replacement *plexusv1.SaveImport) map[string]string {
	return map[string]string{plexusv1.LabelServerID: replacement.Spec.ServerID, plexusv1.LabelOwnerUserID: replacement.Spec.OwnerUserID, plexusv1.LabelSetupID: replacement.Spec.SetupID, plexusv1.LabelGameID: replacement.Spec.GameID, LabelSaveImportID: replacement.Name}
}

func (r *SaveImportReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).For(&plexusv1.SaveImport{}).Owns(&batchv1.Job{}).Complete(r)
}

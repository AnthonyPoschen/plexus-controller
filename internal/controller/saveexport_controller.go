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
	SaveExportUploadURLKey = "upload-url"
	LabelSaveExportID      = "plexus.gg/save-export-id"
	SaveExportFinalizer    = "plexus.gg/save-export-cleanup"
	saveExporterContainer  = "save-exporter"
	saveExportSourceVolume = "save-source"
	saveExportSourcePath   = "/source"
	saveExportFreshFor     = 2 * time.Minute
)

type SaveExportReconciler struct {
	client.Client
	Scheme        *runtime.Scheme
	ExporterImage string
	Progress      SaveExportProgressReader
	Now           func() time.Time
}

// +kubebuilder:rbac:groups=plexus.gg,resources=saveexports,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=saveexports/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=plexus.gg,resources=saveexports/finalizers,verbs=update
// +kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
// +kubebuilder:rbac:groups="",resources=secrets,verbs=get;delete
// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=pods/log,verbs=get

func (r *SaveExportReconciler) Reconcile(ctx context.Context, request ctrl.Request) (ctrl.Result, error) {
	var export plexusv1.SaveExport
	if err := r.Get(ctx, request.NamespacedName, &export); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	if !export.DeletionTimestamp.IsZero() {
		diagnostics, err := r.cleanup(ctx, &export, true)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(diagnostics) != 0 {
			export.Status.Phase = plexusv1.SaveExportFailed
			export.Status.Stage = "cleanup"
			export.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
			if err := r.Status().Update(ctx, &export); err != nil {
				return ctrl.Result{}, err
			}
		}
		if controllerutil.RemoveFinalizer(&export, SaveExportFinalizer) {
			return ctrl.Result{}, r.Update(ctx, &export)
		}
		return ctrl.Result{}, nil
	}
	if controllerutil.AddFinalizer(&export, SaveExportFinalizer) {
		if err := r.Update(ctx, &export); err != nil {
			return ctrl.Result{}, err
		}
	}
	if export.Status.Phase == plexusv1.SaveExportSucceeded || export.Status.Phase == plexusv1.SaveExportFailed || export.Status.Phase == plexusv1.SaveExportExpired {
		removeJob := !export.Spec.ExpiresAt.After(now)
		diagnostics, err := r.cleanup(ctx, &export, removeJob)
		if err != nil {
			return ctrl.Result{}, err
		}
		if len(diagnostics) != 0 && export.Status.Message != boundedDiagnostic(strings.Join(diagnostics, "; ")) {
			export.Status.Stage = "cleanup"
			export.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
			if err := r.Status().Update(ctx, &export); err != nil {
				return ctrl.Result{}, err
			}
		}
		if removeJob {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{RequeueAfter: max(time.Second, export.Spec.ExpiresAt.Sub(now))}, nil
	}
	if !export.Spec.ExpiresAt.After(now) {
		return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportExpired, "expired", "The save export authorization expired before completion")
	}
	definition, err := r.authorize(ctx, &export, now)
	if err != nil {
		return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportFailed, "authorization", err.Error())
	}

	var job batchv1.Job
	err = r.Get(ctx, request.NamespacedName, &job)
	if errors.IsNotFound(err) {
		if err := r.Create(ctx, r.jobFor(&export, definition, now)); err != nil {
			return ctrl.Result{}, err
		}
		startedAt := metav1.NewTime(now)
		export.Status = plexusv1.SaveExportStatus{Phase: plexusv1.SaveExportRunning, ProgressPercent: 0, Stage: "starting", ArchiveName: definition.SaveExport.ArchiveName, StartedAt: &startedAt, Message: "The exporter Job is starting"}
		if err := r.Status().Update(ctx, &export); err != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	if metav1.IsControlledBy(&job, &export) == false {
		return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportFailed, "cleanup", "A same-name Job has different ownership; it was left untouched")
	}
	if job.Status.Succeeded > 0 {
		result, found := r.jobTerminationResult(ctx, &job)
		if found == false || result.Stage != "complete" || result.ArchiveBytes <= 0 {
			return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportFailed, "observation", "The exporter completed without valid archive size metadata")
		}
		export.Status.ArchiveBytes = result.ArchiveBytes
		return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportSucceeded, "complete", "The hosted save archive is ready to download")
	}
	if job.Status.Failed > 0 {
		result, found := r.jobTerminationResult(ctx, &job)
		if found == false || validFailureStage(result.Stage) == false {
			return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportFailed, "job", "The exporter Job failed without a stage-specific diagnostic")
		}
		message := fmt.Sprintf("Save export failed during %s: %s", result.Stage, result.Message)
		return ctrl.Result{}, r.finish(ctx, &export, plexusv1.SaveExportFailed, result.Stage, boundedDiagnostic(message))
	}
	if export.Status.Phase != plexusv1.SaveExportRunning {
		startedAt := metav1.NewTime(now)
		export.Status = plexusv1.SaveExportStatus{Phase: plexusv1.SaveExportRunning, ProgressPercent: 0, Stage: "starting", ArchiveName: definition.SaveExport.ArchiveName, StartedAt: &startedAt, Message: "The exporter Job is starting"}
		if err := r.Status().Update(ctx, &export); err != nil {
			return ctrl.Result{}, err
		}
	}
	if updated, err := r.observeProgress(ctx, &export, &job); err != nil {
		ctrl.LoggerFrom(ctx).Error(err, "unable to observe bounded save export progress", "saveExport", export.Name)
	} else if updated {
		if err := r.Status().Update(ctx, &export); err != nil {
			return ctrl.Result{}, err
		}
	}
	return ctrl.Result{RequeueAfter: min(5*time.Second, export.Spec.ExpiresAt.Sub(now))}, nil
}

func (r *SaveExportReconciler) observeProgress(ctx context.Context, export *plexusv1.SaveExport, job *batchv1.Job) (bool, error) {
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
	if found == false || latest.ProgressPercent <= export.Status.ProgressPercent {
		return false, nil
	}
	export.Status.Phase = plexusv1.SaveExportRunning
	export.Status.ProgressPercent = latest.ProgressPercent
	export.Status.Stage = latest.Stage
	export.Status.Message = progressMessage(latest)
	return true, nil
}

func progressMessage(progress exporterProgress) string {
	switch progress.Stage {
	case "archive":
		if progress.ProgressPercent < 20 {
			return "Locating the hosted save archive"
		}
		return "The hosted save archive was selected"
	case "validation":
		if progress.ProgressPercent < 50 {
			return "Validating the hosted save archive"
		}
		return "The hosted save archive passed validation"
	case "upload":
		if progress.ProgressPercent < 95 {
			return "Uploading the hosted save archive"
		}
		return "The archive upload was accepted and is finalizing"
	default:
		return "The save export is running"
	}
}

func (r *SaveExportReconciler) authorize(ctx context.Context, export *plexusv1.SaveExport, now time.Time) (games.GameDefinition, error) {
	var gameServer plexusv1.GameServer
	if err := r.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Spec.ServerID}, &gameServer); err != nil {
		return games.GameDefinition{}, fmt.Errorf("owned server runtime was not found")
	}
	if gameServer.Spec.ServerID != export.Spec.ServerID || gameServer.Spec.OwnerUserID != export.Spec.OwnerUserID || gameServer.Spec.SelectedSetup == nil ||
		gameServer.Spec.SelectedSetup.ID != export.Spec.SetupID || gameServer.Spec.SelectedSetup.GameID != export.Spec.GameID {
		return games.GameDefinition{}, fmt.Errorf("server ownership or selected setup changed")
	}
	if gameServer.Spec.DesiredPower != plexusv1.DesiredPowerStopped || gameServer.Status.Phase != plexusv1.GameServerPhaseStopped ||
		gameServer.Status.ObservedGeneration != gameServer.Generation || gameServer.Status.LastObservedAt == nil || observationFresh(now, gameServer.Status.LastObservedAt.Time) == false {
		return games.GameDefinition{}, fmt.Errorf("server is not freshly confirmed stopped")
	}
	definition, err := games.Get(export.Spec.GameID)
	if err != nil || definition.SaveExport == nil {
		return games.GameDefinition{}, fmt.Errorf("selected game does not support save export")
	}
	if r.ExporterImage == "" {
		return games.GameDefinition{}, fmt.Errorf("save exporter image is not configured")
	}
	var secret corev1.Secret
	if err := r.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Spec.UploadURLSecretRef}, &secret); err != nil {
		return games.GameDefinition{}, fmt.Errorf("save export transfer authorization is unavailable")
	}
	labels := secret.Labels
	if labels[plexusv1.LabelServerID] != export.Spec.ServerID || labels[plexusv1.LabelOwnerUserID] != export.Spec.OwnerUserID ||
		labels[plexusv1.LabelSetupID] != export.Spec.SetupID || labels[plexusv1.LabelGameID] != export.Spec.GameID || labels[LabelSaveExportID] != export.Name ||
		secret.Immutable == nil || !*secret.Immutable || secret.Type != corev1.SecretTypeOpaque || len(secret.Data) != 1 || len(secret.Data[SaveExportUploadURLKey]) == 0 {
		return games.GameDefinition{}, fmt.Errorf("save export transfer authorization has different ownership or invalid content")
	}
	return definition, nil
}

func (r *SaveExportReconciler) jobFor(export *plexusv1.SaveExport, definition games.GameDefinition, now time.Time) *batchv1.Job {
	zero := int32(0)
	deadline := int64(export.Spec.ExpiresAt.Sub(now).Seconds())
	runAsNonRoot := true
	readOnlyRoot := true
	allowPrivilegeEscalation := false
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: export.Name, Namespace: export.Namespace, Labels: exportLabels(export)}}
	_ = controllerutil.SetControllerReference(export, job, r.Scheme)
	job.Spec.BackoffLimit = &zero
	job.Spec.ActiveDeadlineSeconds = &deadline
	job.Spec.Template.ObjectMeta.Labels = exportLabels(export)
	job.Spec.Template.Spec.RestartPolicy = corev1.RestartPolicyNever
	job.Spec.Template.Spec.AutomountServiceAccountToken = boolPointer(false)
	job.Spec.Template.Spec.SecurityContext = &corev1.PodSecurityContext{RunAsNonRoot: &runAsNonRoot, SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault}}
	job.Spec.Template.Spec.Containers = []corev1.Container{{
		Name: saveExporterContainer, Image: r.ExporterImage, ImagePullPolicy: corev1.PullIfNotPresent,
		Env: []corev1.EnvVar{
			{Name: "PLEXUS_UPLOAD_URL", ValueFrom: &corev1.EnvVarSource{SecretKeyRef: &corev1.SecretKeySelector{LocalObjectReference: corev1.LocalObjectReference{Name: export.Spec.UploadURLSecretRef}, Key: SaveExportUploadURLKey}}},
			{Name: "PLEXUS_SAVE_SOURCE_LAYOUT", Value: string(definition.SaveExport.SourceLayout)},
			{Name: "PLEXUS_SAVE_SELECTION", Value: string(definition.SaveExport.Selection)},
		},
		SecurityContext: &corev1.SecurityContext{RunAsNonRoot: &runAsNonRoot, ReadOnlyRootFilesystem: &readOnlyRoot, AllowPrivilegeEscalation: &allowPrivilegeEscalation, Capabilities: &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}}},
		Resources:       corev1.ResourceRequirements{Requests: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("25m"), corev1.ResourceMemory: resource.MustParse("64Mi")}, Limits: corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("1"), corev1.ResourceMemory: resource.MustParse("512Mi")}},
		VolumeMounts:    []corev1.VolumeMount{{Name: saveExportSourceVolume, MountPath: saveExportSourcePath, SubPath: definition.SaveExport.PVCSubPath, ReadOnly: true}},
	}}
	job.Spec.Template.Spec.Volumes = []corev1.Volume{{Name: saveExportSourceVolume, VolumeSource: corev1.VolumeSource{PersistentVolumeClaim: &corev1.PersistentVolumeClaimVolumeSource{ClaimName: export.Spec.ServerID, ReadOnly: true}}}}
	return job
}

func (r *SaveExportReconciler) finish(ctx context.Context, export *plexusv1.SaveExport, phase plexusv1.SaveExportPhase, stage string, message string) error {
	finishedAt := metav1.NewTime(time.Now().UTC())
	if r.Now != nil {
		finishedAt = metav1.NewTime(r.Now().UTC())
	}
	export.Status.Phase = phase
	export.Status.Stage = stage
	export.Status.Message = boundedDiagnostic(message)
	export.Status.FinishedAt = &finishedAt
	if phase == plexusv1.SaveExportSucceeded {
		export.Status.ProgressPercent = 100
	}
	if err := r.Status().Update(ctx, export); err != nil {
		return err
	}
	diagnostics, err := r.cleanup(ctx, export, phase == plexusv1.SaveExportExpired)
	if err != nil {
		return err
	}
	if len(diagnostics) == 0 {
		return nil
	}
	export.Status.Stage = "cleanup"
	export.Status.Message = boundedDiagnostic(strings.Join(diagnostics, "; "))
	return r.Status().Update(ctx, export)
}

func (r *SaveExportReconciler) cleanup(ctx context.Context, export *plexusv1.SaveExport, removeJob bool) ([]string, error) {
	diagnostics := []string{}
	var job batchv1.Job
	err := r.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Name}, &job)
	if err == nil && metav1.IsControlledBy(&job, export) == false {
		diagnostics = append(diagnostics, "A same-name Job has different ownership and was left untouched")
	}
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if removeJob && err == nil && metav1.IsControlledBy(&job, export) {
		if err := r.Delete(ctx, &job, client.PropagationPolicy(metav1.DeletePropagationBackground)); err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
	}
	var secret corev1.Secret
	err = r.Get(ctx, client.ObjectKey{Namespace: export.Namespace, Name: export.Spec.UploadURLSecretRef}, &secret)
	if err == nil && saveExportAuthorizationOwnedBy(&secret, export) == false {
		diagnostics = append(diagnostics, "The referenced Secret has different ownership and was left untouched")
	}
	if err != nil && !errors.IsNotFound(err) {
		return nil, err
	}
	if err == nil && saveExportAuthorizationOwnedBy(&secret, export) {
		if err := r.Delete(ctx, &secret); err != nil && !errors.IsNotFound(err) {
			return nil, err
		}
	}
	return diagnostics, nil
}

type exporterTermination struct {
	Stage        string `json:"stage"`
	ArchiveBytes int64  `json:"archiveBytes,omitempty"`
	Message      string `json:"message,omitempty"`
}

func (r *SaveExportReconciler) jobTerminationResult(ctx context.Context, job *batchv1.Job) (exporterTermination, bool) {
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
			if status.Name != saveExporterContainer || status.State.Terminated == nil {
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

func validFailureStage(stage string) bool {
	return stage == "archive" || stage == "validation" || stage == "upload"
}

func boundedDiagnostic(message string) string {
	message = strings.Join(strings.Fields(message), " ")
	if len(message) > 512 {
		return message[:509] + "..."
	}
	return message
}

func saveExportAuthorizationOwnedBy(secret *corev1.Secret, export *plexusv1.SaveExport) bool {
	labels := secret.Labels
	return labels[plexusv1.LabelServerID] == export.Spec.ServerID && labels[plexusv1.LabelOwnerUserID] == export.Spec.OwnerUserID && labels[plexusv1.LabelSetupID] == export.Spec.SetupID && labels[plexusv1.LabelGameID] == export.Spec.GameID && labels[LabelSaveExportID] == export.Name
}

func observationFresh(now time.Time, observed time.Time) bool {
	age := now.Sub(observed)
	return age >= -30*time.Second && age <= saveExportFreshFor
}

func exportLabels(export *plexusv1.SaveExport) map[string]string {
	return map[string]string{plexusv1.LabelServerID: export.Spec.ServerID, plexusv1.LabelOwnerUserID: export.Spec.OwnerUserID, plexusv1.LabelSetupID: export.Spec.SetupID, plexusv1.LabelGameID: export.Spec.GameID, LabelSaveExportID: export.Name}
}

func boolPointer(value bool) *bool { return &value }

func (r *SaveExportReconciler) SetupWithManager(manager ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(manager).For(&plexusv1.SaveExport{}).Owns(&batchv1.Job{}).Complete(r)
}

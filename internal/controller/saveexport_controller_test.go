package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	plexusv1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

func TestSaveExportCreatesPathScopedShortLivedJobOnlyForFreshStoppedSetup(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 0, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = plexusv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	observedAt := metav1.NewTime(now)
	gameServer := &plexusv1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: "games", Generation: 7}, Spec: plexusv1.GameServerSpec{
		ServerID: "server-1", OwnerUserID: "owner-1", DesiredPower: plexusv1.DesiredPowerStopped,
		SelectedSetup: &plexusv1.SelectedSetupSpec{ID: "setup-1", GameID: "factorio"},
	}, Status: plexusv1.GameServerStatus{Phase: plexusv1.GameServerPhaseStopped, ActiveSetupID: "setup-1", ObservedGeneration: 7, LastObservedAt: &observedAt}}
	expiresAt := metav1.NewTime(now.Add(10 * time.Minute))
	export := &plexusv1.SaveExport{ObjectMeta: metav1.ObjectMeta{Name: "export-1", Namespace: "games"}, Spec: plexusv1.SaveExportSpec{
		ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", UploadURLSecretRef: "export-1-upload", ExpiresAt: expiresAt,
	}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "export-1-upload", Namespace: "games", Labels: map[string]string{
		plexusv1.LabelServerID: "server-1", plexusv1.LabelOwnerUserID: "owner-1", plexusv1.LabelSetupID: "setup-1", plexusv1.LabelGameID: "factorio", LabelSaveExportID: "export-1",
	}}, Immutable: boolPointer(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SaveExportUploadURLKey: []byte("https://objects.example/upload?signature=secret")}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(gameServer, export, secret).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, ExporterImage: "registry.example/save-exporter:v1", Now: func() time.Time { return now }}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "games", Name: "export-1"}}); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "games", Name: "export-1"}, &job); err != nil {
		t.Fatal(err)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if len(container.VolumeMounts) != 1 || container.VolumeMounts[0].SubPath != "saves" || container.VolumeMounts[0].MountPath != "/source" || !container.VolumeMounts[0].ReadOnly {
		t.Fatalf("export job was not restricted to adapter save path: %#v", container.VolumeMounts)
	}
	if container.Image != "registry.example/save-exporter:v1" || container.Env[0].ValueFrom.SecretKeyRef.Name != "export-1-upload" {
		t.Fatalf("export job lost managed image or secret authorization: %#v", container)
	}
	if len(container.Env) != 3 || container.Env[1].Name != "PLEXUS_SAVE_SOURCE_LAYOUT" || container.Env[1].Value != "archive-directory" || container.Env[2].Name != "PLEXUS_SAVE_SELECTION" || container.Env[2].Value != "latest-modified-archive" {
		t.Fatalf("export job lost adapter-owned save selection: %#v", container.Env)
	}
}

func TestSaveExportRejectsDeletingGameServerBeforeCreatingJob(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 15, 0, 0, time.UTC)
	export, gameServer, secret := authorizedSaveExportFixtures(now)
	deletedAt := metav1.NewTime(now.Add(-time.Second))
	gameServer.DeletionTimestamp = &deletedAt
	gameServer.Finalizers = []string{"plexus.gg/test"}
	scheme := saveExportTestScheme(t)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, gameServer, secret).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, ExporterImage: "registry.example/save-exporter:v1", Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: export.Namespace, Name: export.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveExport
	if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveExportFailed || observed.Status.Stage != "authorization" || !strings.Contains(observed.Status.Message, "being deleted") {
		t.Fatalf("deleting server authorization status = %#v", observed.Status)
	}
	if err := client.Get(context.Background(), request.NamespacedName, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("deleting server received exporter Job: %v", err)
	}
}

func TestExpiredSaveExportCleansManagedResourcesIdempotently(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 30, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = plexusv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	expiresAt := metav1.NewTime(now.Add(-time.Second))
	export := &plexusv1.SaveExport{ObjectMeta: metav1.ObjectMeta{Name: "export-expired", Namespace: "games", UID: types.UID("export-uid")}, Spec: plexusv1.SaveExportSpec{ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", UploadURLSecretRef: "expired-upload", ExpiresAt: expiresAt}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "expired-upload", Namespace: "games", Labels: exportLabels(export)}}
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: "export-expired", Namespace: "games", OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveExport", Name: export.Name, UID: export.UID, Controller: &controller}}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, secret, job).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "games", Name: "export-expired"}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveExport
	if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveExportExpired || observed.Status.FinishedAt == nil {
		t.Fatalf("expired operation did not retain useful status: %#v", observed.Status)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "games", Name: "expired-upload"}, &corev1.Secret{}); !apierrors.IsNotFound(err) {
		t.Fatalf("temporary authorization was not cleaned: %v", err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, &batchv1.Job{}); !apierrors.IsNotFound(err) {
		t.Fatalf("short-lived export job was not cleaned: %v", err)
	}
}

func TestExpiredSaveExportCannotDeleteUnownedSecret(t *testing.T) {
	now := time.Date(2026, 8, 15, 1, 45, 0, 0, time.UTC)
	scheme := runtime.NewScheme()
	_ = plexusv1.AddToScheme(scheme)
	_ = corev1.AddToScheme(scheme)
	_ = batchv1.AddToScheme(scheme)
	expiresAt := metav1.NewTime(now.Add(-time.Second))
	export := &plexusv1.SaveExport{ObjectMeta: metav1.ObjectMeta{Name: "malicious", Namespace: "games"}, Spec: plexusv1.SaveExportSpec{ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", UploadURLSecretRef: "setup-1-secrets", ExpiresAt: expiresAt}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "setup-1-secrets", Namespace: "games", Labels: map[string]string{plexusv1.LabelSetupID: "setup-1"}}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, secret).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: "games", Name: "malicious"}}
	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: "games", Name: "setup-1-secrets"}, &corev1.Secret{}); err != nil {
		t.Fatalf("unowned Secret was deleted: %v", err)
	}
	var observed plexusv1.SaveExport
	if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Stage != "cleanup" || observed.Status.Message == "" {
		t.Fatalf("ownership conflict was not retained diagnostically: %#v", observed.Status)
	}
}

func TestDeletingSaveExportSkipsHostileReferencesAndReleasesFinalizer(t *testing.T) {
	now := time.Date(2026, 8, 15, 2, 0, 0, 0, time.UTC)
	scheme := saveExportTestScheme(t)
	deletedAt := metav1.NewTime(now)
	export := &plexusv1.SaveExport{ObjectMeta: metav1.ObjectMeta{
		Name: "hostile", Namespace: "games", Finalizers: []string{SaveExportFinalizer}, DeletionTimestamp: &deletedAt,
	}, Spec: plexusv1.SaveExportSpec{ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", UploadURLSecretRef: "hostile-secret", ExpiresAt: metav1.NewTime(now.Add(time.Minute))}}
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: export.Name, Namespace: export.Namespace}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: export.Spec.UploadURLSecretRef, Namespace: export.Namespace}}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, job, secret).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: export.Namespace, Name: export.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	if err := client.Get(context.Background(), request.NamespacedName, &batchv1.Job{}); err != nil {
		t.Fatalf("hostile Job was deleted: %v", err)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: export.Namespace, Name: secret.Name}, &corev1.Secret{}); err != nil {
		t.Fatalf("hostile Secret was deleted: %v", err)
	}
	var observed plexusv1.SaveExport
	err := client.Get(context.Background(), request.NamespacedName, &observed)
	if err == nil && controllerutil.ContainsFinalizer(&observed, SaveExportFinalizer) {
		t.Fatalf("hostile references wedged finalizer: %#v", observed.Finalizers)
	}
	if err != nil && !apierrors.IsNotFound(err) {
		t.Fatal(err)
	}
}

func TestSaveExportCompletionPropagatesArchiveSizeAndRetainsJobUntilExpiry(t *testing.T) {
	now := time.Date(2026, 8, 15, 2, 15, 0, 0, time.UTC)
	scheme := saveExportTestScheme(t)
	export, gameServer, secret := authorizedSaveExportFixtures(now)
	export.UID = "export-uid"
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: export.Name, Namespace: export.Namespace, UID: "job-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveExport", Name: export.Name, UID: export.UID, Controller: &controller}}}, Status: batchv1.JobStatus{Succeeded: 1}}
	pod := terminatedExporterPod(job, `{"stage":"complete","archiveBytes":12345}`)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, gameServer, secret, job, pod).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, ExporterImage: "registry.example/save-exporter:v1", Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: export.Namespace, Name: export.Name}}

	if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveExport
	if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveExportSucceeded || observed.Status.ArchiveBytes != 12345 || observed.Status.ProgressPercent != 100 || observed.Status.Stage != "complete" {
		t.Fatalf("truthful completion metadata was not retained: %#v", observed.Status)
	}
	if err := client.Get(context.Background(), request.NamespacedName, &batchv1.Job{}); err != nil {
		t.Fatalf("Job diagnostic was deleted before expiry: %v", err)
	}
}

func TestSaveExportPreservesBoundedStageSpecificFailures(t *testing.T) {
	for _, stage := range []string{"archive", "validation", "upload"} {
		t.Run(stage, func(t *testing.T) {
			now := time.Date(2026, 8, 15, 2, 30, 0, 0, time.UTC)
			scheme := saveExportTestScheme(t)
			export, gameServer, secret := authorizedSaveExportFixtures(now)
			export.Name += "-" + stage
			export.Spec.UploadURLSecretRef = export.Name + "-upload"
			export.UID = types.UID(export.Name + "-uid")
			secret.Name = export.Spec.UploadURLSecretRef
			secret.Labels = exportLabels(export)
			controller := true
			job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: export.Name, Namespace: export.Namespace, UID: types.UID(export.Name + "-job"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveExport", Name: export.Name, UID: export.UID, Controller: &controller}}}, Status: batchv1.JobStatus{Failed: 1}}
			pod := terminatedExporterPod(job, `{"stage":"`+stage+`","message":"safe diagnostic"}`)
			client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, gameServer, secret, job, pod).Build()
			reconciler := SaveExportReconciler{Client: client, Scheme: scheme, ExporterImage: "registry.example/save-exporter:v1", Now: func() time.Time { return now }}
			request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: export.Namespace, Name: export.Name}}
			if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
				t.Fatal(err)
			}
			var observed plexusv1.SaveExport
			if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
				t.Fatal(err)
			}
			if observed.Status.Phase != plexusv1.SaveExportFailed || observed.Status.Stage != stage || !strings.Contains(observed.Status.Message, "safe diagnostic") || len(observed.Status.Message) > 512 {
				t.Fatalf("stage failure was not retained safely: %#v", observed.Status)
			}
			if err := client.Get(context.Background(), request.NamespacedName, &batchv1.Job{}); err != nil {
				t.Fatalf("failed Job was deleted before observation window: %v", err)
			}
		})
	}
}

func saveExportTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{plexusv1.AddToScheme, corev1.AddToScheme, batchv1.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	return scheme
}

func authorizedSaveExportFixtures(now time.Time) (*plexusv1.SaveExport, *plexusv1.GameServer, *corev1.Secret) {
	observedAt := metav1.NewTime(now)
	gameServer := &plexusv1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: "games", Generation: 7}, Spec: plexusv1.GameServerSpec{ServerID: "server-1", OwnerUserID: "owner-1", DesiredPower: plexusv1.DesiredPowerStopped, SelectedSetup: &plexusv1.SelectedSetupSpec{ID: "setup-1", GameID: "factorio"}}, Status: plexusv1.GameServerStatus{Phase: plexusv1.GameServerPhaseStopped, ObservedGeneration: 7, LastObservedAt: &observedAt}}
	export := &plexusv1.SaveExport{ObjectMeta: metav1.ObjectMeta{Name: "export", Namespace: "games"}, Spec: plexusv1.SaveExportSpec{ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", UploadURLSecretRef: "export-upload", ExpiresAt: metav1.NewTime(now.Add(10 * time.Minute))}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: export.Spec.UploadURLSecretRef, Namespace: export.Namespace, Labels: exportLabels(export)}, Immutable: boolPointer(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SaveExportUploadURLKey: []byte("https://objects.example/upload")}}
	return export, gameServer, secret
}

func terminatedExporterPod(job *batchv1.Job, message string) *corev1.Pod {
	controller := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: saveExporterContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}}}}}}
}

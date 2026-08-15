package controller

import (
	"context"
	"strings"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	plexusv1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

func TestSaveImportCreatesWritablePathScopedJobOnlyForFreshStoppedSetup(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	scheme := saveExportTestScheme(t)
	replacement, gameServer, secret := authorizedSaveImportFixtures(now)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveImport{}).WithObjects(gameServer, replacement, secret).Build()
	reconciler := SaveImportReconciler{Client: client, Scheme: scheme, ImporterImage: "registry.example/save-importer:v1", Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}}); err != nil {
		t.Fatal(err)
	}
	var job batchv1.Job
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}, &job); err != nil {
		t.Fatal(err)
	}
	if len(job.Spec.Template.Spec.Containers) != 1 {
		t.Fatalf("unexpected containers: %#v", job.Spec.Template.Spec.Containers)
	}
	container := job.Spec.Template.Spec.Containers[0]
	if len(container.VolumeMounts) != 2 || container.VolumeMounts[0].SubPath != "saves" || container.VolumeMounts[0].MountPath != "/target" || container.VolumeMounts[0].ReadOnly {
		t.Fatalf("import Job did not mount adapter save data writable: %#v", container.VolumeMounts)
	}
	if container.VolumeMounts[1].MountPath != "/work" {
		t.Fatalf("import Job is missing a staging workspace: %#v", container.VolumeMounts)
	}
	if len(job.Spec.Template.Spec.Volumes) != 2 || job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim == nil || job.Spec.Template.Spec.Volumes[0].PersistentVolumeClaim.ReadOnly || job.Spec.Template.Spec.Volumes[1].EmptyDir == nil {
		t.Fatalf("import Job volumes were not adapter-scoped: %#v", job.Spec.Template.Spec.Volumes)
	}
	var observed plexusv1.GameServer
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: gameServer.Namespace, Name: gameServer.Name}, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Spec.DesiredPower != plexusv1.DesiredPowerStopped {
		t.Fatalf("import started the Server: %#v", observed.Spec)
	}
}

func TestSaveImportRejectsDeletingGameServerBeforeCreatingJob(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	replacement, gameServer, secret := authorizedSaveImportFixtures(now)
	deletedAt := metav1.NewTime(now)
	gameServer.DeletionTimestamp = &deletedAt
	gameServer.Finalizers = []string{"plexus.gg/test"}
	client := fake.NewClientBuilder().WithScheme(saveExportTestScheme(t)).WithStatusSubresource(&plexusv1.SaveImport{}).WithObjects(replacement, gameServer, secret).Build()
	reconciler := SaveImportReconciler{Client: client, Scheme: saveExportTestScheme(t), ImporterImage: "registry.example/save-importer:v1", Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}}); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveImport
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveImportFailed || observed.Status.Stage != "authorization" || !strings.Contains(observed.Status.Message, "being deleted") {
		t.Fatalf("deleting GameServer was not rejected: %#v", observed.Status)
	}
}

func TestSaveImportCompletionLeavesServerStoppedAndDoesNotClaimSnapshot(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	scheme := saveExportTestScheme(t)
	replacement, gameServer, secret := authorizedSaveImportFixtures(now)
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: replacement.Name, Namespace: replacement.Namespace, UID: "job-uid", OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveImport", Name: replacement.Name, UID: replacement.UID, Controller: &controller}}}, Status: batchv1.JobStatus{Succeeded: 1}}
	pod := terminatedImporterPod(job, `{"stage":"complete","archiveBytes":12345}`)
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveImport{}).WithObjects(replacement, gameServer, secret, job, pod).Build()
	reconciler := SaveImportReconciler{Client: client, Scheme: scheme, ImporterImage: "registry.example/save-importer:v1", Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}}); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveImport
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveImportSucceeded || observed.Status.ArchiveBytes != 12345 || observed.Status.ProgressPercent != 100 || !strings.Contains(observed.Status.Message, "remains stopped") {
		t.Fatalf("successful replacement was not recorded honestly: %#v", observed.Status)
	}
	if strings.Contains(strings.ToLower(observed.Status.Message), "snapshot") {
		t.Fatalf("success claimed a recovery snapshot: %q", observed.Status.Message)
	}
	var server plexusv1.GameServer
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: gameServer.Namespace, Name: gameServer.Name}, &server); err != nil {
		t.Fatal(err)
	}
	if server.Spec.DesiredPower != plexusv1.DesiredPowerStopped {
		t.Fatalf("successful replacement started the Server: %#v", server.Spec)
	}
}

func TestSaveImportFailureDoesNotClaimAutomaticRecoverySnapshot(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	replacement, gameServer, secret := authorizedSaveImportFixtures(now)
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{Name: replacement.Name, Namespace: replacement.Namespace, UID: types.UID(replacement.Name + "-job"), OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveImport", Name: replacement.Name, UID: replacement.UID, Controller: &controller}}}, Status: batchv1.JobStatus{Failed: 1}}
	pod := terminatedImporterPod(job, `{"stage":"replace","message":"safe diagnostic"}`)
	client := fake.NewClientBuilder().WithScheme(saveExportTestScheme(t)).WithStatusSubresource(&plexusv1.SaveImport{}).WithObjects(replacement, gameServer, secret, job, pod).Build()
	reconciler := SaveImportReconciler{Client: client, Scheme: saveExportTestScheme(t), ImporterImage: "registry.example/save-importer:v1", Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}}); err != nil {
		t.Fatal(err)
	}
	var observed plexusv1.SaveImport
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}, &observed); err != nil {
		t.Fatal(err)
	}
	if observed.Status.Phase != plexusv1.SaveImportFailed || observed.Status.Stage != "replace" || !strings.Contains(observed.Status.Message, "safe diagnostic") {
		t.Fatalf("stage failure was not retained safely: %#v", observed.Status)
	}
	if !strings.Contains(observed.Status.Message, "did not create an automatic recovery snapshot") {
		t.Fatalf("failure implied automatic recovery: %q", observed.Status.Message)
	}
}

func TestSaveImportDoesNotMutateDesiredPowerOnAuthorizationFailure(t *testing.T) {
	now := time.Date(2026, 8, 15, 4, 0, 0, 0, time.UTC)
	replacement, gameServer, secret := authorizedSaveImportFixtures(now)
	gameServer.Status.Phase = plexusv1.GameServerPhaseRunning
	client := fake.NewClientBuilder().WithScheme(saveExportTestScheme(t)).WithStatusSubresource(&plexusv1.SaveImport{}, &plexusv1.GameServer{}).WithObjects(replacement, gameServer, secret).Build()
	reconciler := SaveImportReconciler{Client: client, Scheme: saveExportTestScheme(t), ImporterImage: "registry.example/save-importer:v1", Now: func() time.Time { return now }}
	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{NamespacedName: types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}}); err != nil {
		t.Fatal(err)
	}
	var server plexusv1.GameServer
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: gameServer.Namespace, Name: gameServer.Name}, &server); err != nil {
		t.Fatal(err)
	}
	if server.Spec.DesiredPower != plexusv1.DesiredPowerStopped {
		t.Fatalf("authorization failure changed desired power: %#v", server.Spec)
	}
	if err := client.Get(context.Background(), types.NamespacedName{Namespace: replacement.Namespace, Name: replacement.Name}, &batchv1.Job{}); err == nil {
		t.Fatal("import Job was created for a running Server")
	} else if errors.IsNotFound(err) == false {
		t.Fatal(err)
	}
}

func authorizedSaveImportFixtures(now time.Time) (*plexusv1.SaveImport, *plexusv1.GameServer, *corev1.Secret) {
	observedAt := metav1.NewTime(now)
	gameServer := &plexusv1.GameServer{ObjectMeta: metav1.ObjectMeta{Name: "server-1", Namespace: "games", Generation: 7}, Spec: plexusv1.GameServerSpec{ServerID: "server-1", OwnerUserID: "owner-1", DesiredPower: plexusv1.DesiredPowerStopped, SelectedSetup: &plexusv1.SelectedSetupSpec{ID: "setup-1", GameID: "factorio"}}, Status: plexusv1.GameServerStatus{Phase: plexusv1.GameServerPhaseStopped, ObservedGeneration: 7, LastObservedAt: &observedAt}}
	replacement := &plexusv1.SaveImport{ObjectMeta: metav1.ObjectMeta{Name: "import", Namespace: "games", UID: "import-uid"}, Spec: plexusv1.SaveImportSpec{ServerID: "server-1", OwnerUserID: "owner-1", SetupID: "setup-1", GameID: "factorio", DownloadURLSecretRef: "import-download", ArchiveName: "copper-works.zip", ExpiresAt: metav1.NewTime(now.Add(10 * time.Minute))}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: replacement.Spec.DownloadURLSecretRef, Namespace: replacement.Namespace, Labels: importLabels(replacement)}, Immutable: boolPointer(true), Type: corev1.SecretTypeOpaque, Data: map[string][]byte{SaveImportDownloadURLKey: []byte("https://objects.example/download")}}
	return replacement, gameServer, secret
}

func terminatedImporterPod(job *batchv1.Job, message string) *corev1.Pod {
	controller := true
	return &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}, Status: corev1.PodStatus{ContainerStatuses: []corev1.ContainerStatus{{Name: saveImporterContainer, State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{Message: message}}}}}}
}

package controller

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	plexusv1 "github.com/AnthonyPoschen/plexus-controller/api/v1alpha1"
)

func TestParseLatestExporterProgressIsBoundedAndMonotonic(t *testing.T) {
	logs := "not-json\n" +
		`{"stage":"archive","progressPercent":20}` + "\n" +
		`{"stage":"upload","progressPercent":80,"uploadURL":"https://objects.example/?secret=leak"}` + "\n" +
		`{"stage":"upload","progressPercent":101}` + "\n" +
		`{"stage":"unknown","progressPercent":99}` + "\n"
	progress, found, err := parseLatestExporterProgress(logs)
	if err != nil {
		t.Fatal(err)
	}
	if found == false || progress != (exporterProgress{Stage: "upload", ProgressPercent: 80}) {
		t.Fatalf("latest safe progress = %#v found=%v", progress, found)
	}
}

func TestRunningSaveExportPublishesOnlyAdvancingCoarseProgress(t *testing.T) {
	now := time.Date(2026, 8, 15, 3, 0, 0, 0, time.UTC)
	scheme := saveExportTestScheme(t)
	export, gameServer, secret := authorizedSaveExportFixtures(now)
	export.UID = "export-uid"
	startedAt := metav1.NewTime(now.Add(-time.Minute))
	export.Status = plexusv1.SaveExportStatus{Phase: plexusv1.SaveExportRunning, ProgressPercent: 20, Stage: "archive", StartedAt: &startedAt}
	controller := true
	job := &batchv1.Job{ObjectMeta: metav1.ObjectMeta{
		Name: export.Name, Namespace: export.Namespace, UID: "job-uid",
		OwnerReferences: []metav1.OwnerReference{{APIVersion: "plexus.gg/v1alpha1", Kind: "SaveExport", Name: export.Name, UID: export.UID, Controller: &controller}},
	}}
	pod := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{Name: job.Name + "-pod", Namespace: job.Namespace, Labels: map[string]string{"job-name": job.Name}, OwnerReferences: []metav1.OwnerReference{{APIVersion: "batch/v1", Kind: "Job", Name: job.Name, UID: job.UID, Controller: &controller}}}}
	progress := &fakeProgressReader{update: exporterProgress{Stage: "validation", ProgressPercent: 50}, found: true}
	client := fake.NewClientBuilder().WithScheme(scheme).WithStatusSubresource(&plexusv1.SaveExport{}).WithObjects(export, gameServer, secret, job, pod).Build()
	reconciler := SaveExportReconciler{Client: client, Scheme: scheme, ExporterImage: "registry.example/save-exporter:v1", Progress: progress, Now: func() time.Time { return now }}
	request := ctrl.Request{NamespacedName: types.NamespacedName{Namespace: export.Namespace, Name: export.Name}}

	assertProgress := func(stage string, percent int32) {
		t.Helper()
		if _, err := reconciler.Reconcile(context.Background(), request); err != nil {
			t.Fatal(err)
		}
		var observed plexusv1.SaveExport
		if err := client.Get(context.Background(), request.NamespacedName, &observed); err != nil {
			t.Fatal(err)
		}
		if observed.Status.Stage != stage || observed.Status.ProgressPercent != percent || observed.Status.Phase != plexusv1.SaveExportRunning {
			t.Fatalf("observed progress = %#v, want stage=%s percent=%d", observed.Status, stage, percent)
		}
	}

	assertProgress("validation", 50)
	progress.update = exporterProgress{Stage: "archive", ProgressPercent: 20}
	assertProgress("validation", 50)
	progress.update = exporterProgress{Stage: "upload", ProgressPercent: 80}
	assertProgress("upload", 80)
}

type fakeProgressReader struct {
	update exporterProgress
	found  bool
	err    error
}

func (reader *fakeProgressReader) Latest(context.Context, string, string) (exporterProgress, bool, error) {
	return reader.update, reader.found, reader.err
}

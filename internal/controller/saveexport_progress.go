package controller

import (
	"bufio"
	"context"
	"encoding/json"
	"strings"

	corev1 "k8s.io/api/core/v1"
	typedcorev1 "k8s.io/client-go/kubernetes/typed/core/v1"
)

const (
	saveExportProgressTailLines  = int64(16)
	saveExportProgressLimitBytes = int64(8192)
)

type exporterProgress struct {
	Stage           string `json:"stage"`
	ProgressPercent int32  `json:"progressPercent"`
}

type SaveExportProgressReader interface {
	Latest(context.Context, string, string) (exporterProgress, bool, error)
}

type podLogProgressReader struct {
	pods typedcorev1.PodsGetter
}

func NewPodLogProgressReader(pods typedcorev1.PodsGetter) SaveExportProgressReader {
	return &podLogProgressReader{pods: pods}
}

func (reader *podLogProgressReader) Latest(ctx context.Context, namespace string, podName string) (exporterProgress, bool, error) {
	data, err := reader.pods.Pods(namespace).GetLogs(podName, &corev1.PodLogOptions{
		Container:  saveExporterContainer,
		TailLines:  int64Pointer(saveExportProgressTailLines),
		LimitBytes: int64Pointer(saveExportProgressLimitBytes),
	}).DoRaw(ctx)
	if err != nil {
		return exporterProgress{}, false, err
	}
	return parseLatestExporterProgress(string(data))
}

func parseLatestExporterProgress(logs string) (exporterProgress, bool, error) {
	latest := exporterProgress{}
	found := false
	scanner := bufio.NewScanner(strings.NewReader(logs))
	for scanner.Scan() {
		var candidate exporterProgress
		if json.Unmarshal(scanner.Bytes(), &candidate) != nil || validProgress(candidate) == false {
			continue
		}
		if found == false || candidate.ProgressPercent > latest.ProgressPercent {
			latest, found = candidate, true
		}
	}
	return latest, found, scanner.Err()
}

func validProgress(progress exporterProgress) bool {
	return validFailureStage(progress.Stage) && progress.ProgressPercent > 0 && progress.ProgressPercent < 100
}

func int64Pointer(value int64) *int64 { return &value }

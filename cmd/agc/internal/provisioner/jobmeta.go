package provisioner

import corev1 "k8s.io/api/core/v1"

// jobMeta holds the GitHub Actions context of the job a worker pod runs, from the
// scale-set assignment (ScaleSetJob.jobMeta). Every field is best-effort: an empty one
// is omitted from the pod.
type jobMeta struct {
	runID      string // numeric GitHub run ID, e.g. "12345678"
	repository string // "owner/repo"
	jobName    string // job name from workflow YAML, e.g. "build"
}

// Worker-pod annotation keys for the GitHub Actions context of the job a pod runs.
// Informational for an operator reading `kubectl describe pod` — with one exception:
// AnnotationRunID and AnnotationRepository are also the durable record eviction
// recovery reads back to name the run to re-run, since the AGC keeps no in-process
// job state (Q417). Exported for that reason. The job-name key is read too, by
// flaky-job and out-of-memory detection and by ReissueReplacement.
const (
	AnnotationRunID      = "actions-gateway.com/run-id"
	AnnotationRepository = "actions-gateway.com/repository"
	annotationJobName    = "actions-gateway.com/job-name"
)

// podAnnotations returns the actions-gateway.com/* annotations to stamp on
// worker pods. Only non-empty fields are included, so a job missing one carries no
// zero-value key.
func (m jobMeta) podAnnotations() map[string]string {
	a := make(map[string]string, 3)
	if m.runID != "" {
		a[AnnotationRunID] = m.runID
	}
	if m.repository != "" {
		a[AnnotationRepository] = m.repository
	}
	if m.jobName != "" {
		a[annotationJobName] = m.jobName
	}
	if len(a) == 0 {
		return nil
	}
	return a
}

// ReissueReplacement names the worker GitHub's re-issue of pod's job was given, or ""
// when there is none (Q1361). GitHub withdraws a scale-set assignment no runner takes
// within five minutes and re-issues the same workflow job under a new job ID, so the
// replacement carries pod's run ID and job name and was created after it. A pod whose
// containers ever started is not one GitHub gave up waiting on, and a pod missing
// either annotation has nothing tying it to a later worker, so both report "".
func ReissueReplacement(pod *corev1.Pod, pods []corev1.Pod) string {
	runID, jobName := pod.Annotations[AnnotationRunID], pod.Annotations[annotationJobName]
	if runID == "" || runID == "0" || jobName == "" || podEverStarted(pod) {
		return ""
	}
	for i := range pods {
		other := &pods[i]
		if other.Name == pod.Name || !other.DeletionTimestamp.IsZero() ||
			!other.CreationTimestamp.After(pod.CreationTimestamp.Time) {
			continue
		}
		if other.Annotations[AnnotationRunID] == runID && other.Annotations[annotationJobName] == jobName {
			return other.Name
		}
	}
	return ""
}

package provisioner

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
// job state (Q417). Exported for that reason; the cosmetic job-name key
// is not read by anything.
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

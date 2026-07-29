package provisioner

import (
	"encoding/json"
	"strconv"
	"strings"
)

// acquirePayload extracts eviction-retry fields from the raw AcquireJob response.
//
// Run identity travels in the serialised `github` context, not in the job
// variables: a real acquirejob body carries contextData.github.run_id and
// contextData.github.repository, and no system.github.run_id at all. That is
// measured, not assumed — testdata/job_payload.json is a redacted capture of a live
// response, and payload_groundtruth_test.go asserts against it (Q495). The
// variables map and the top-level run_id are retained as tolerated fallbacks for
// payload shapes that do carry them (the fakes, and any protocol variant that
// populates them).
type acquirePayload struct {
	RunID       int64                       `json:"run_id"` // top-level field; may be absent
	Variables   map[string]variableEnvValue `json:"variables"`
	ContextData struct {
		GitHub dictionaryContextData `json:"github"`
	} `json:"contextData"`
}

type variableEnvValue struct {
	Value string `json:"value"`
}

// dictionaryContextData is the wire form of the runner SDK's DictionaryContextData:
// a type tag plus an ordered key/value list — {"t":2,"d":[{"k":"run_id","v":"123"}]}
// — rather than a plain JSON object. Values are heterogeneous (most are strings,
// but `event` is a nested dictionary and `ref_protected` a bool), so each is held
// raw and coerced on read.
type dictionaryContextData struct {
	Pairs []contextDataPair `json:"d"`
}

type contextDataPair struct {
	Key   string          `json:"k"`
	Value json.RawMessage `json:"v"`
}

// str returns the string held under key, or "" when the key is absent or its value
// is not a scalar. A number is accepted and rendered in its JSON form, so a run_id
// sent unquoted still resolves.
func (d dictionaryContextData) str(key string) string {
	for i := range d.Pairs {
		if d.Pairs[i].Key != key {
			continue
		}
		var s string
		if err := json.Unmarshal(d.Pairs[i].Value, &s); err == nil {
			return s
		}
		var n json.Number
		if err := json.Unmarshal(d.Pairs[i].Value, &n); err == nil {
			return n.String()
		}
		return ""
	}
	return ""
}

// runIdentity resolves the run ID and "owner/repo" that name this job's workflow
// run, trying each known source in turn and taking the first that answers.
//
// Both the eviction-retry path (repoInfo) and the worker-pod annotations
// (jobMetaFrom) read it, so the two cannot again disagree about where identity
// comes from — they previously read the same two sources independently, and when
// neither source turned out to exist in a real payload, real worker pods carried no
// run-id annotation and eviction recovery had no run to re-run (Q495).
func (ap *acquirePayload) runIdentity() (runID, repository string) {
	runID = runIDCandidate(ap.ContextData.GitHub.str("run_id"))
	repository = ap.ContextData.GitHub.str("repository")
	if ap.Variables != nil {
		if runID == "" {
			runID = runIDCandidate(ap.Variables["system.github.run_id"].Value)
		}
		if repository == "" {
			repository = ap.Variables["system.github.repository"].Value
		}
	}
	if runID == "" && ap.RunID != 0 {
		runID = strconv.FormatInt(ap.RunID, 10)
	}
	return
}

// runIDCandidate returns s when it is a plausible GitHub run ID — a run of digits —
// and "" otherwise. A malformed value is dropped rather than carried, so it neither
// displaces a good value from a later source nor reaches the pod annotation the
// scale-set tier reads back as a run identity.
func runIDCandidate(s string) string {
	if _, err := strconv.ParseUint(s, 10, 64); err != nil {
		return ""
	}
	return s
}

// repoInfo extracts the owner, repo, and run ID from the parsed payload.
// Returns empty strings/zero if the fields are not present.
func (ap *acquirePayload) repoInfo() (owner, repo string, runID int64) {
	rawRunID, repository := ap.runIdentity()
	if parts := strings.SplitN(repository, "/", 2); len(parts) == 2 {
		owner, repo = parts[0], parts[1]
	}
	// runIdentity has already rejected anything non-numeric, so a parse failure
	// here means "absent" and leaves runID at 0 — what handleEviction treats as
	// an unknown run.
	if n, err := strconv.ParseInt(rawRunID, 10, 64); err == nil {
		runID = n
	}
	return
}

// jobMeta holds GitHub Actions context extracted from the AcquireJob payload.
// All fields are best-effort: an absent or malformed payload leaves them empty,
// which is benign — the annotations are simply omitted from the pod.
type jobMeta struct {
	runID      string // numeric GitHub run ID, e.g. "12345678"
	repository string // "owner/repo"
	jobName    string // job name from workflow YAML, e.g. "build"
	workflow   string // workflow name, e.g. "CI"
}

// jobMetaFrom extracts the job annotation fields from a parsed AcquireJob payload.
func jobMetaFrom(ap acquirePayload) jobMeta {
	var m jobMeta
	m.runID, m.repository = ap.runIdentity()
	if ap.Variables != nil {
		m.jobName = ap.Variables["system.github.job"].Value
		m.workflow = ap.Variables["system.github.workflow"].Value
	}
	// The two cosmetic fields have their own fallbacks: a real payload carries the
	// job name as a variable but the workflow only in the github context.
	if m.jobName == "" {
		m.jobName = ap.ContextData.GitHub.str("job")
	}
	if m.workflow == "" {
		m.workflow = ap.ContextData.GitHub.str("workflow")
	}
	return m
}

// Worker-pod annotation keys for the GitHub Actions context of the job a pod runs.
// Informational for an operator reading `kubectl describe pod` — with one exception:
// on the scale-set tier AnnotationRunID and AnnotationRepository are also the durable
// record eviction recovery reads back to name the run to re-run, since that tier keeps
// no in-process job state (Q417). Exported for that reason; the two cosmetic keys are
// not read by anything.
const (
	AnnotationRunID      = "actions-gateway.com/run-id"
	AnnotationRepository = "actions-gateway.com/repository"
	annotationJobName    = "actions-gateway.com/job-name"
	annotationWorkflow   = "actions-gateway.com/workflow"
)

// podAnnotations returns the actions-gateway.com/* annotations to stamp on
// worker pods. Only non-empty fields are included so pods created from
// minimal/stub payloads don't carry zero-value keys.
func (m jobMeta) podAnnotations() map[string]string {
	a := make(map[string]string, 4)
	if m.runID != "" {
		a[AnnotationRunID] = m.runID
	}
	if m.repository != "" {
		a[AnnotationRepository] = m.repository
	}
	if m.jobName != "" {
		a[annotationJobName] = m.jobName
	}
	if m.workflow != "" {
		a[annotationWorkflow] = m.workflow
	}
	if len(a) == 0 {
		return nil
	}
	return a
}

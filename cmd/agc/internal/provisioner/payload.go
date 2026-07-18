package provisioner

import (
	"fmt"
	"strings"
)

// acquirePayload extracts eviction-retry fields from the raw AcquireJob response.
// GitHub Actions embeds workflow context in the "variables" map as
// {"system.github.run_id": {"value":"12345"}, "system.github.repository": {"value":"owner/repo"}}.
type acquirePayload struct {
	RunID     int64                       `json:"run_id"` // top-level field; may be absent
	Variables map[string]variableEnvValue `json:"variables"`
}

type variableEnvValue struct {
	Value string `json:"value"`
}

// repoInfo extracts the owner, repo, and run ID from the parsed payload.
// Returns empty strings/zero if the fields are not present.
func (ap *acquirePayload) repoInfo() (owner, repo string, runID int64) {
	if ap.Variables != nil {
		if v, ok := ap.Variables["system.github.repository"]; ok {
			parts := strings.SplitN(v.Value, "/", 2)
			if len(parts) == 2 {
				owner, repo = parts[0], parts[1]
			}
		}
		if v, ok := ap.Variables["system.github.run_id"]; ok {
			// A malformed run_id leaves runID at 0, falling back to ap.RunID below.
			if _, err := fmt.Sscanf(v.Value, "%d", &runID); err != nil {
				runID = 0
			}
		}
	}
	if runID == 0 {
		runID = ap.RunID
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
	if ap.Variables != nil {
		if v, ok := ap.Variables["system.github.run_id"]; ok {
			m.runID = v.Value
		}
		if v, ok := ap.Variables["system.github.repository"]; ok {
			m.repository = v.Value
		}
		if v, ok := ap.Variables["system.github.job"]; ok {
			m.jobName = v.Value
		}
		if v, ok := ap.Variables["system.github.workflow"]; ok {
			m.workflow = v.Value
		}
	}
	if m.runID == "" && ap.RunID != 0 {
		m.runID = fmt.Sprintf("%d", ap.RunID)
	}
	return m
}

// podAnnotations returns the actions-gateway.com/* annotations to stamp on
// worker pods. Only non-empty fields are included so pods created from
// minimal/stub payloads don't carry zero-value keys.
func (m jobMeta) podAnnotations() map[string]string {
	a := make(map[string]string, 4)
	if m.runID != "" {
		a["actions-gateway.com/run-id"] = m.runID
	}
	if m.repository != "" {
		a["actions-gateway.com/repository"] = m.repository
	}
	if m.jobName != "" {
		a["actions-gateway.com/job-name"] = m.jobName
	}
	if m.workflow != "" {
		a["actions-gateway.com/workflow"] = m.workflow
	}
	if len(a) == 0 {
		return nil
	}
	return a
}

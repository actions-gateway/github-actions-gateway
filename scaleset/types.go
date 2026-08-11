// Package scaleset implements a GAG-owned client for GitHub's runner-scale-set
// message-queue protocol — the protocol modern Actions Runner Controller (ARC)
// uses to acquire jobs, adopted here to remove the classic broker protocol's
// many-acquirers fan-out race by construction (Q264, Option E).
//
// The client mirrors the wire TYPES of the official, Public-Preview
// github.com/actions/scaleset package for wire parity, but is a clean
// reimplementation rather than a vendored dependency (decision U6-C, Q264 plan
// §5a): GAG must own the auto-assign semantics upstream neither documents nor
// answers for (upstream issue #107), so the fake, the tests, and the invariants
// encode what the live probes settled (Q264 plan §2a/§2b). Every wire flow here
// was implemented and live-validated first in cmd/probe/scaleset.go
// (Investigations E and E2, 2026-07-04); this package promotes those flows.
//
// That relationship is now the other way round: cmd/probe drives this package
// rather than its own copy of it, so a live probe run is evidence about the code
// GAG ships (Q362). The probe reports the raw wire the typed API hides through
// ResponseObserver, and compares un-modelled routes against this client's own
// construction through RawServiceCall — both exist for that caller.
//
// # Two-hop auth bootstrap
//
// The protocol pivots off the public REST API after two bootstrap hops:
//
//  1. Registration token — POST {api}/orgs|repos/.../actions/runners/registration-token
//     with a GitHub App installation token → short-lived registration token.
//  2. Admin connection — POST {api}/actions/runner-registration with the
//     nonstandard header "Authorization: RemoteAuth <registration token>" →
//     the runtime-discovered Actions Service tenant URL and an admin JWT.
//
// Everything else targets {actionsServiceURL}/_apis/runtime/... with
// "Authorization: Bearer <admin JWT>" and api-version=6.0-preview.
//
// # Token matrix (Q264 plan §2.5)
//
//	App installation token — minted by githubapp; authorizes the registration-token call.
//	Registration token     — authorizes the RemoteAuth runner-registration hop.
//	Admin JWT              — authorizes scale-set CRUD, sessions, and generatejitconfig.
//	                         ~1h TTL, but observed live to expire within ~17 min, so it
//	                         is refreshed lazily ~60s before expiry (mandatory — §2b-7).
//	Queue access token     — minted by session create/refresh; authorizes the queue
//	                         long-poll AND acquirejobs (NOT the admin JWT). Refreshed
//	                         via session PATCH on a 401 from the queue.
//	JIT config             — one-shot per job; consumed by the runner pod's own session.
//
// # Security invariants
//
// All endpoints are GitHub-hosted and must traverse the per-tenant egress proxy
// exactly as the classic broker client does: the client's HTTP clients clone
// http.DefaultTransport (which main() patches with the proxy CA early — Q219), so
// they inherit the proxy trust bundle rather than bypassing it. The App/admin/queue
// tokens never leave the AGC; only the per-job JIT config reaches a worker pod
// (staged into its Secret by the provisioner, today's surface — Q264 plan §3).
package scaleset

import (
	"encoding/json"
	"fmt"
	"strconv"
	"time"
)

// AdminConnection is the result of the RemoteAuth runner-registration hop: the
// runtime-discovered Actions Service tenant URL and the admin JWT that authorizes
// scale-set CRUD, sessions, and generatejitconfig.
type AdminConnection struct {
	// URL is the Actions Service tenant base (e.g.
	// https://broker.actions.githubusercontent.com/rest on the current
	// broker-host backend, or https://pipelines.actions.githubusercontent.com/<tenant>
	// on the ARC-era backend). Subsequent _apis/runtime calls target it.
	URL string `json:"url"`
	// Token is the admin JWT (~1h nominal TTL; observed to expire within ~17 min).
	Token string `json:"token"`
}

// Label is one runner-scale-set label — a runs-on match target. A scale set may carry
// several: the first is its own name, and the rest are additional targets the Actions
// Service matches the way it matches a plain self-hosted runner's labels. Measured
// 2026-08-11 against ARC 0.14.0 (2026-03-19), which added them upstream in
// actions/actions-runner-controller#4408; before that the name label was the only one,
// which is what Q264 plan §2.1 recorded.
//
// A GitHub Enterprise Server appliance below 3.21 keeps only the name label unless a
// site admin enables DistributedTask.AllowRunnerScaleSetCustomLabels, and drops the
// rest with no error — so a caller that needs them must compare the returned label set
// against the one it sent (Q726).
type Label struct {
	Name string `json:"name"`
	Type string `json:"type"` // "System" for every runs-on match target
}

// RunnerSetting carries per-scale-set runner behaviour. GAG always registers
// ephemeral scale sets (single-use runners), matching the classic tier.
type RunnerSetting struct {
	Ephemeral bool `json:"ephemeral"`
}

// RunnerScaleSet is the scale-set object: one per RunnerGroup, created once, then
// reused for every job (contrast the classic tier's N pre-registered agents).
type RunnerScaleSet struct {
	ID            int            `json:"id,omitempty"`
	Name          string         `json:"name"`
	RunnerGroupID int            `json:"runnerGroupId"`
	Labels        []Label        `json:"labels,omitempty"`
	RunnerSetting *RunnerSetting `json:"runnerSetting,omitempty"`
}

// RunnerGroup is a runner-group reference resolved by name. Org-scoped scale sets
// are gated by the group's policy (a public repo needs allows_public_repositories);
// repo-scoped scale sets bypass groups entirely (Q264 plan §2a-6).
type RunnerGroup struct {
	ID   int    `json:"id"`
	Name string `json:"name"`
}

// RunnerScaleSetSession is the message-queue session: one active per scale set.
// A second create conflicts (SessionConflictError) until the first is deleted or
// expires. The queue token authorizes the long-poll and acquirejobs; PATCH
// refreshes it on a 401.
type RunnerScaleSetSession struct {
	SessionID               string                   `json:"sessionId"`
	OwnerName               string                   `json:"ownerName"`
	MessageQueueURL         string                   `json:"messageQueueUrl"`
	MessageQueueAccessToken string                   `json:"messageQueueAccessToken"`
	Statistics              *RunnerScaleSetStatistic `json:"statistics,omitempty"`
}

// RunnerScaleSetStatistic is the server-authoritative snapshot carried on every
// session response and queue message. Scale on TotalAssignedJobs — the ARC
// listener's formula is clamp(TotalAssignedJobs, minRunners, maxRunners) — rather
// than by counting individual messages (Q264 plan §2.2).
type RunnerScaleSetStatistic struct {
	TotalAvailableJobs     int `json:"totalAvailableJobs"`
	TotalAcquiredJobs      int `json:"totalAcquiredJobs"`
	TotalAssignedJobs      int `json:"totalAssignedJobs"`
	TotalRunningJobs       int `json:"totalRunningJobs"`
	TotalRegisteredRunners int `json:"totalRegisteredRunners"`
	TotalBusyRunners       int `json:"totalBusyRunners"`
	TotalIdleRunners       int `json:"totalIdleRunners"`
}

// MessageType is the type of a job message carried in a queue message's body.
type MessageType string

const (
	// MessageTypeJobAvailable offers a queued job for claiming. On GHES the client
	// must respond with acquirejobs for the offered RunnerRequestID; on the dotcom
	// broker-host backend this message does not appear (jobs auto-assign) — see
	// Client.AcquireJobs and the one-rule acquisition model (Q264 plan §5a-U8).
	MessageTypeJobAvailable MessageType = "JobAvailable"
	// MessageTypeJobAssigned confirms a job is assigned to this scale set — the
	// authoritative "provision a runner" signal, whether it followed an explicit
	// acquire (GHES) or arrived directly under the capacity gate (dotcom).
	MessageTypeJobAssigned MessageType = "JobAssigned"
	// MessageTypeJobStarted reports that a runner picked up its assigned job.
	MessageTypeJobStarted MessageType = "JobStarted"
	// MessageTypeJobCompleted reports a job's terminal result back to the
	// listener — a signal the classic protocol never gives the AGC (Q264 plan §2b-6).
	MessageTypeJobCompleted MessageType = "JobCompleted"
)

// RunnerScaleSetMessage is one envelope returned by the queue long-poll. Body is a
// JSON array of JobMessage entries (batched typed job messages); Statistics is the
// snapshot to reconcile against. MessageID is the cursor: advance lastMessageId past
// it AND delete the message to acknowledge (Q264 plan §2.2).
type RunnerScaleSetMessage struct {
	MessageID   int64                    `json:"messageId"`
	MessageType string                   `json:"messageType"`
	Body        string                   `json:"body"`
	Statistics  *RunnerScaleSetStatistic `json:"statistics,omitempty"`
}

// Jobs decodes the message body into its batched JobMessage entries. A message
// with an empty body yields no entries and no error.
func (m *RunnerScaleSetMessage) Jobs() ([]JobMessage, error) {
	if m.Body == "" {
		return nil, nil
	}
	var jobs []JobMessage
	if err := json.Unmarshal([]byte(m.Body), &jobs); err != nil {
		return nil, fmt.Errorf("scaleset: decode message body: %w", err)
	}
	return jobs, nil
}

// JobMessage is one typed entry in a queue message's batched body. The populated
// fields depend on MessageType.
type JobMessage struct {
	MessageType MessageType `json:"messageType"`
	// RunnerRequestID identifies a job for the acquire flow. On JobAvailable it is
	// the id to pass to acquirejobs; on the auto-assign backend JobAssigned carries 0.
	RunnerRequestID int64 `json:"runnerRequestId"`
	// AcquireJobURL, when present on a JobAvailable entry, is the authoritative
	// acquire endpoint for that job — preferred over the static _apis/runtime route,
	// which 404s on the broker-host backend (Q264 plan §2a-3, jobTest note).
	AcquireJobURL string `json:"acquireJobUrl,omitempty"`
	// JobID is the job's UUID (present on JobAssigned/JobStarted/JobCompleted).
	JobID string `json:"jobId,omitempty"`
	// RunnerID / RunnerName identify the runner that holds a started/completed job.
	RunnerID   int64  `json:"runnerId,omitempty"`
	RunnerName string `json:"runnerName,omitempty"`
	// Result is the terminal result on JobCompleted (e.g. "succeeded", "failed").
	Result string `json:"result,omitempty"`

	// Run identity, carried on every message type by the protocol's JobMessageBase
	// (see RunIdentity for why these are modelled and how absence is handled).
	//
	// OwnerName is the repository owner (org or user) and RepositoryName the bare
	// repository name — the protocol splits what the classic broker payload delivers
	// as one "owner/repo" variable.
	OwnerName      string `json:"ownerName,omitempty"`
	RepositoryName string `json:"repositoryName,omitempty"`
	// WorkflowRunID is the workflow run the job belongs to — the run_id the
	// rerun-failed-jobs REST call takes.
	WorkflowRunID int64 `json:"workflowRunId,omitempty"`
	// JobDisplayName is the job's human-readable name from the workflow YAML.
	JobDisplayName string `json:"jobDisplayName,omitempty"`
}

// RunIdentity returns the workflow run this job belongs to, as the (owner, repo,
// run_id) triple the GitHub REST API addresses a run by, and whether all three are
// present. A false ok means the message did not carry a complete identity, and any
// caller that needs one (eviction recovery, which must POST rerun-failed-jobs for a
// specific run) has to degrade rather than guess.
//
// Why the fields are modelled at all: this client mirrors the wire types of the
// official actions/scaleset package (see the package doc), whose JobMessageBase —
// embedded in JobAvailable, JobAssigned, JobStarted, and JobCompleted alike —
// carries ownerName, repositoryName, and workflowRunId. The live probe of the
// dotcom broker-host backend corroborates that the raw body really is a
// JobMessageBase: it observed scaleSetAssignTime, another base field this client
// does not model (Q264 plan §2a-3).
//
// A live probe against the dotcom broker-host backend confirmed all three on a real
// JobAssigned on 2026-07-26, with workflowRunId matching the dispatched run exactly
// (see the plan doc's "Measured live" section).
//
// Why ok is still a return value rather than an assumption: that is one observation on
// one backend, and it says nothing about GHES, another event type, or a future protocol
// revision. Treating identity as optional costs one branch and makes an absence
// observable (a logged warning and an Event) instead of producing a rerun against run 0.
func (j JobMessage) RunIdentity() (owner, repo, runID string, ok bool) {
	if j.OwnerName == "" || j.RepositoryName == "" || j.WorkflowRunID == 0 {
		return "", "", "", false
	}
	return j.OwnerName, j.RepositoryName, strconv.FormatInt(j.WorkflowRunID, 10), true
}

// AvailableJobIDs returns the RunnerRequestIDs of the JobAvailable entries in jobs
// — exactly the ids to claim via Client.AcquireJobs. On the auto-assign backend
// there are none (jobs arrive as JobAssigned directly); this is the GHES path of
// the one-rule acquisition model (Q264 plan §5a-U8).
func AvailableJobIDs(jobs []JobMessage) []int64 {
	var ids []int64
	for _, j := range jobs {
		if j.MessageType == MessageTypeJobAvailable {
			ids = append(ids, j.RunnerRequestID)
		}
	}
	return ids
}

// AssignedJobs returns the JobAssigned entries in jobs — the authoritative
// "provision a runner" set, regardless of whether they followed an explicit
// acquire (GHES) or auto-assignment (dotcom). Treating JobAssigned as
// authoritative is what makes the dotcom-vs-GHES skew one code path, not a fork
// (Q264 plan §5a-U8).
func AssignedJobs(jobs []JobMessage) []JobMessage {
	var out []JobMessage
	for _, j := range jobs {
		if j.MessageType == MessageTypeJobAssigned {
			out = append(out, j)
		}
	}
	return out
}

// JITRunnerConfig is the generatejitconfig result: a base64 blob bundling the
// runner's .runner + .credentials (+ RSA params) that a runner pod consumes with
// run.sh --jitconfig, plus the pre-registered runner's id and name.
type JITRunnerConfig struct {
	Runner struct {
		ID   int    `json:"id"`
		Name string `json:"name"`
	} `json:"runner"`
	EncodedJITConfig string `json:"encodedJITConfig"`
}

// ExpiresWithin reports whether the admin connection's JWT expires within d of
// now. The client refreshes the connection when this is true (lazy pre-expiry
// refresh). A token whose exp cannot be parsed is treated as already expired so
// the client re-mints rather than presenting a token it cannot reason about.
func (c *AdminConnection) ExpiresWithin(d time.Duration) bool {
	exp, err := parseJWTExpiry(c.Token)
	if err != nil {
		return true
	}
	return time.Until(exp) <= d
}

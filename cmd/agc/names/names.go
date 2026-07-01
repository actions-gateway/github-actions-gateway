// Package names provides the canonical on-cluster resource names — and the
// pinned actions/runner version — shared between the Gateway Manager Controller
// (GMC) and the Actions Gateway Controller (AGC).
//
// Both controllers must use the same string for the AGC Deployment name, its
// ServiceAccount, the NetworkPolicy that grants it Kubernetes API egress, and the
// app.kubernetes.io/managed-by label on worker pods and agent Secrets. A single
// constant here is the single source of truth; changing it in only one place would
// silently break the NetworkPolicy pod-selector match at runtime.
//
// The same single-source-of-truth discipline applies to the runner version: it
// must be identical in the default worker image the AGC pulls, the agent.version
// the AGC sends to GitHub at session creation, and the FROM tag in
// cmd/worker/Dockerfile. RunnerVersion below drives the first two; the lockstep
// test in runner_version_test.go pins it to the third so a Dependabot Dockerfile
// bump that forgets the constant fails CI.
package names

// ControllerName is the canonical name used for:
//   - the AGC Deployment (and its app: label)
//   - the AGC ServiceAccount, Role, and RoleBinding
//   - the NetworkPolicy that selects AGC pods (app: actions-gateway-controller)
//   - the value of app.kubernetes.io/managed-by on worker pods and agent Secrets
const ControllerName = "actions-gateway-controller"

// WorkerSAName is the ServiceAccount name assigned to worker pods. The GMC
// creates this ServiceAccount in each tenant namespace and injects it into the
// AGC Deployment via the WORKER_SERVICE_ACCOUNT env var. Both sides must agree
// on this name; a single constant here prevents silent drift.
const WorkerSAName = "actions-gateway-worker"

// RunnerVersion is the pinned actions/runner version embedded in the default
// worker image. It is the single source of truth for the runner version:
//   - cmd/agc/internal/provisioner derives DefaultWorkerImage from it.
//   - cmd/gmc injects it into the AGC Deployment as GITHUB_RUNNER_VERSION, which
//     the AGC forwards as agent.version on CreateSession. GitHub validates the
//     runner version at session creation, so an empty or wrong value risks
//     rejection (the runner-version contract — see Q71/Q118 in docs/STATUS.md
//     and the milestone-4 plan note).
//
// It MUST equal the FROM tag in cmd/worker/Dockerfile; the lockstep test in
// runner_version_test.go enforces that so a future bump cannot drift.
const RunnerVersion = "2.335.1"

// WorkerImageRepo is the actions/runner image repository. It matches the ARC
// gha-runner-scale-set default so tenants copy-pasting from ARC manifests see
// the same image name.
const WorkerImageRepo = "ghcr.io/actions/actions-runner"

// WorkerImageDigest is the multi-arch manifest-index digest of WorkerImageRepo
// at RunnerVersion (covers linux/amd64 + linux/arm64; see the multi-arch
// image plan, Q97). It MUST equal the @sha256 in
// cmd/worker/Dockerfile's FROM line; the lockstep test enforces that.
//
// Re-resolve on a version bump with:
//
//	docker buildx imagetools inspect ghcr.io/actions/actions-runner:<X.Y.Z>
//
// using the top-level "Digest:" (the OCI image index), not a per-platform one.
const WorkerImageDigest = "sha256:08c30b0a7105f64bddfc485d2487a22aa03932a791402393352fdf674bda2c29"

// DefaultWorkerImage is the fully digest-pinned default worker image
// ("<repo>:<version>@<digest>") the AGC pulls when neither the per-RunnerGroup
// workerImage field nor the WORKER_IMAGE environment variable (set by the GMC on
// the AGC Deployment) is set. Digest-pinning is
// secure-by-default: a tag alone is mutable.
const DefaultWorkerImage = WorkerImageRepo + ":" + RunnerVersion + "@" + WorkerImageDigest

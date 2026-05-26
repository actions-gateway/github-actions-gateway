// Package names provides the canonical on-cluster resource names shared between
// the Gateway Manager Controller (GMC) and the Actions Gateway Controller (AGC).
//
// Both controllers must use the same string for the AGC Deployment name, its
// ServiceAccount, the NetworkPolicy that grants it Kubernetes API egress, and the
// app.kubernetes.io/managed-by label on worker pods and agent Secrets. A single
// constant here is the single source of truth; changing it in only one place would
// silently break the NetworkPolicy pod-selector match at runtime.
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

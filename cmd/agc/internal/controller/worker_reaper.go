package controller

// Reap reasons, used as the `reason` label of
// actions_gateway_worker_pods_reaped_total.
const (
	reapReasonCompletedTTL    = "completed_ttl"
	reapReasonPendingDeadline = "pending_deadline"
	// reapReasonOrphanedRunning labels a pod deleted because it was still Running
	// long after its job went terminal at GitHub — a worker that never received its
	// job, or one held open past the runner's exit (Q420).
	reapReasonOrphanedRunning = "orphaned_running"
	// reapReasonLifetimeExceeded labels a pod deleted after the kubelet killed it
	// for exceeding spec.maxWorkerLifetime (the pod's activeDeadlineSeconds). It is
	// split out from completed_ttl so an operator debugging a killed long job sees
	// the lifetime cap as the cause rather than a mystery termination (Q438).
	reapReasonLifetimeExceeded = "lifetime_exceeded"
	// reapReasonCompletedPending labels a pod deleted because it was still Pending
	// after its job went terminal at GitHub — a worker that can no longer start,
	// because the completion reclaimed the JIT-config Secret it mounts. It is split
	// out from pending_deadline so an operator is not sent after a scheduling problem
	// that does not exist (Q575).
	reapReasonCompletedPending = "completed_pending"
	// reapReasonIdleRunner labels a scale-set pod deleted because its runner had been
	// up for idleRunnerGrace without starting a job and GitHub agreed it was idle — a
	// worker no completion stamp will ever reach, such as one built after an AGC
	// restart for a job another runner already took (Q1360).
	reapReasonIdleRunner = "idle_runner"
	// reapReasonGatewayDeleted labels a pod deleted because its ActionsGateway is
	// terminating: the AGC is the pods' only reaper and is about to be torn down with
	// the gateway, so it reaps them itself rather than leave them pinning a node with
	// nothing left to release them (Q547).
	reapReasonGatewayDeleted = "gateway_deleted"
)

// podReasonDeadlineExceeded is the Pod.Status.Reason the kubelet sets when it kills
// a pod for exceeding activeDeadlineSeconds. It is disjoint from the eviction path's
// "Evicted" (see eviction_scaleset.go), so a lifetime kill can never be mistaken for
// a node-pressure eviction and can never trigger a spurious rerun-failed-jobs.
const podReasonDeadlineExceeded = "DeadlineExceeded"

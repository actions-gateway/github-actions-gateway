package controller

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/actions-gateway/github-actions-gateway/agc/internal/provisioner"
	"github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// gatewayTerminating reports whether the RunnerSet's referenced ActionsGateway
// exists and carries a deletion timestamp.
//
// The trigger is deliberately a terminating gateway rather than a missing one. A
// gateway that is gone is the resting state after teardown and also the gap an
// operator sees between deleting and re-applying a gateway; reaping there would
// destroy live workers on a recreate, and a restarted AGC cannot tell that gap from
// a real teardown. A deletion timestamp is unambiguous, and it is set before the GMC
// deletes anything — which is the only window in which the AGC still holds the RBAC
// to reap (Q547).
func gatewayTerminating(ctx context.Context, c client.Client, rs *v2alpha1.RunnerSet) (bool, error) {
	var gw v2alpha1.ActionsGateway
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Spec.GatewayRef.Name}
	if err := c.Get(ctx, key, &gw); err != nil {
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return false, fmt.Errorf("read ActionsGateway: %w", err)
	}
	return !gw.DeletionTimestamp.IsZero(), nil
}

// reconcileGatewayTerminating quiesces a RunnerSet whose ActionsGateway is being
// deleted: it stops both acquisition tiers so no further job is taken, then deletes
// every worker pod the set still has.
//
// The AGC is the only reaper those pods have — they are owned by the RunnerSet, which
// survives gateway deletion by design, so nothing else would ever release them and
// their node-disruption-safety annotations pin the node until the kubelet's
// activeDeadlineSeconds fires up to maxWorkerLifetime later (Q547). The GMC holds its
// teardown open until status.activeJobs/pendingJobs reach zero, so this runs while the
// AGC still has its ServiceAccount and RoleBinding.
//
// It runs before reference resolution: a set whose template or proxy is also missing
// still has to reap. Deletes are issued, not awaited — once a pod carries a deletion
// timestamp the kubelet finishes it with no controller involved, which is exactly the
// property being restored here.
func (r *RunnerSetReconciler) reconcileGatewayTerminating(ctx context.Context, log *slog.Logger, rs *v2alpha1.RunnerSet) (ctrl.Result, error) {
	key := types.NamespacedName{Namespace: rs.Namespace, Name: rs.Name}

	// Capture the deregistration hook before stopping the listener: the scale-set
	// client belongs to the listener handle, and stopScaleSetListener drops it.
	deregister := r.deregisterScaleSetRunner(rs, log)
	r.stopMultiplexer(key)
	r.stopScaleSetListener(key)

	reaped, err := reapAllWorkerPodsByLabel(ctx, r.Client, rs.Namespace, rs.Name,
		provisioner.LabelRunnerSet, reapReasonGatewayDeleted, log, r.Metrics,
		reapHooks{deregisterRunner: deregister})
	if err != nil {
		return ctrl.Result{}, err
	}
	if reaped > 0 {
		// Operator-visible: a job running at teardown is killed with the gateway, and
		// that has to be legible as a deliberate reap rather than a mystery deletion.
		r.recordEvent(rs, corev1.EventTypeWarning, "WorkerPodsReapedOnGatewayTeardown", "ReapWorkerPods",
			"ActionsGateway %q is terminating; deleted %d worker pod(s) because this AGC is their only reaper "+
				"and is torn down with the gateway; any job they were running is lost",
			rs.Spec.GatewayRef.Name, reaped)
	}

	r.setReadyCondition(rs, false, v2alpha1.ReasonGatewayTerminating,
		fmt.Sprintf("ActionsGateway %q is being deleted; acquisition stopped and worker pods reaped", rs.Spec.GatewayRef.Name))
	// No listeners running and no worker pods left, so every capacity/sidecar alarm
	// describes a state that no longer exists — the same clearing the unresolved-
	// reference path does.
	r.setReapBlockingSidecarStatus(rs, nil, nil)
	r.clearWorkerCapacityConditions(rs)
	rs.Status.ActiveSessions = 0
	rs.Status.ActiveJobs = 0
	rs.Status.PendingJobs = 0
	clearAdvertisedCapacity(rs)
	rs.Status.ObservedGeneration = rs.Generation
	if err := r.Status().Update(ctx, rs); err != nil && !apierrors.IsConflict(err) {
		return ctrl.Result{}, err
	}
	return ctrl.Result{}, nil
}

package controller

import (
	"net/http"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/event"
)

// conditionUpdate is sent from listener goroutines to the reconciler via a channel.
type conditionUpdate struct {
	namespace string
	name      string
	condition metav1.Condition
}

// channelConditionUpdater implements runnercore.ConditionUpdater. After enqueuing, it
// wakes the reconciler (wake) so the pushed condition is drained promptly rather than
// waiting for the next worker-Pod event or the resync period (Q333).
type channelConditionUpdater struct {
	ch   chan<- conditionUpdate
	wake chan<- event.GenericEvent
}

func (u *channelConditionUpdater) SetCondition(namespace, name string, cond metav1.Condition) {
	select {
	case u.ch <- conditionUpdate{namespace: namespace, name: name, condition: cond}:
	default:
		// Drop if channel is full to avoid blocking listener goroutines.
	}
	wakeReconciler(u.wake, namespace, name)
}

// BrokerConfig holds the connection parameters for the broker client used by
// listener goroutines.
type BrokerConfig struct {
	BrokerURL     string
	RunnerVersion string
	RunnerOS      string
	RunnerArch    string
	UseV2Flow     bool
	HTTPClient    *http.Client
	// IdleThreshold is the number of consecutive empty polls before a burst
	// listener goroutine shuts down. 0 means the default (50).
	IdleThreshold int
	// RenewJobInterval is the cadence of the per-job RenewJob renewal loop.
	// 0 means the default (60s).
	RenewJobInterval time.Duration
	// FanoutCompletion enables the Q260 Option A: the winner of a fanned-out job fans
	// completejob out to every deduped sibling delivery on completion so GitHub does
	// not cancel the whole job at its ~15-minute unstarted-job timeout. ON by default;
	// the run service's per-delivery completion semantics were live-confirmed by the
	// re-route #5 dogfood experiment — see listener.Config.FanoutCompletion.
	FanoutCompletion bool
}

// defaultBaselineRecheckInterval is the cadence at which a RunnerSet is requeued
// while its multiplexer is below the desired listener count, used when
// RunnerSetReconciler.BaselineRecheckInterval is unset. 15s revives a dead
// permanent baseline well inside the e2e job budget while keeping reconcile
// churn modest at the per-tenant scale of a single AGC instance (Q137).
const defaultBaselineRecheckInterval = 15 * time.Second

package controller

import (
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

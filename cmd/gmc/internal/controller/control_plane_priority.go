package controller

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	schedulingv1 "k8s.io/api/scheduling/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

// +kubebuilder:rbac:groups=scheduling.k8s.io,resources=priorityclasses,verbs=get;list;watch

// applyControlPlanePriorityClass gives a controller pod that names no PriorityClass
// the platform's control-plane class (--control-plane-priority-class), so it sits
// above every worker class and no worker can preempt it (Q1351). A class the
// tenant named on spec.scheduling stays. An empty class leaves the pod as built.
func applyControlPlanePriorityClass(spec *corev1.PodSpec, class string) {
	if class == "" || spec.PriorityClassName != "" {
		return
	}
	spec.PriorityClassName = class
}

// ReadPriorityClassValues lists the cluster's PriorityClasses into the value map
// allowlist.Misordered compares.
func ReadPriorityClassValues(ctx context.Context, r client.Reader) (allowlist.ClassValues, error) {
	var list schedulingv1.PriorityClassList
	if err := r.List(ctx, &list); err != nil {
		return allowlist.ClassValues{}, fmt.Errorf("list PriorityClasses: %w", err)
	}
	cv := allowlist.ClassValues{Values: make(map[string]int32, len(list.Items))}
	for i := range list.Items {
		pc := &list.Items[i]
		cv.Values[pc.Name] = pc.Value
		if pc.GlobalDefault {
			cv.GlobalDefault = pc.Name
		}
	}
	return cv, nil
}

// VerifyPriorityOrder refuses a start on which a worker could outrank a controller
// (Q1351): the control-plane class must exist, and allowlist.CheckOrder must find
// no worker-reachable class at or above any control-plane class.
func VerifyPriorityOrder(ctx context.Context, r client.Reader, worker, infra *allowlist.PriorityClassAllowlist, controlPlaneClass string) error {
	cv, err := ReadPriorityClassValues(ctx, r)
	if err != nil {
		return err
	}
	if _, ok := cv.Values[controlPlaneClass]; controlPlaneClass != "" && !ok {
		return fmt.Errorf("--control-plane-priority-class %q names no PriorityClass; create it, or every AGC "+
			"and egress proxy pod that names no class is refused by the apiserver", controlPlaneClass)
	}
	return allowlist.MisorderedError(allowlist.CheckOrder(cv, worker, infra))
}

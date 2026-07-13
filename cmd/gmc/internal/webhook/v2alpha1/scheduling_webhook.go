/*
Copyright 2026.

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

    http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package v2alpha1

import (
	"fmt"

	agcv2alpha1 "github.com/actions-gateway/github-actions-gateway/api/v2alpha1"
	"github.com/actions-gateway/github-actions-gateway/gmc/internal/allowlist"
)

// validateSchedulingPriorityClass rejects a spec.scheduling.priorityClassName on an
// INFRA pod — the EgressProxy pool pod or the v2 ActionsGateway control-plane pod —
// that is not on the infra-only allowlist (--allowed-infra-priority-classes, Q284).
//
// This is a SEPARATE gate from the worker-facing PriorityClass allowlist that
// validatePodTemplatePriorityClass (Q289) applies. The two allowlists are disjoint by
// construction (the GMC refuses to start if they intersect): an infra class must not be
// nameable from a worker pod, or a tenant could lift its workers to infra priority and
// preempt other tenants' proxy pods, inverting the ordering the infra gate protects.
//
// Priority is the one PodScheduling field behind a gate. The rest of the block
// (nodeSelector/tolerations/affinity/topologySpreadConstraints) is tenant-settable and
// ungated: those weaken attribution, not isolation, and cannot preempt another tenant.
// priorityClassName is a cluster-wide, cross-tenant preemption lever — and because
// `system-*` classes are not kube-system-scoped (a pod in any namespace may name
// system-cluster-critical), an ungated name is a cluster-wide preemption escape.
//
// The empty string means the pod names no class (the Kubernetes default) and is always
// permitted, so an unset --allowed-infra-priority-classes forbids every named class
// without forbidding an ordinary unprioritized infra pod. A nil scheduling block, or a
// nil allowlist paired with an empty name, both pass.
func validateSchedulingPriorityClass(s *agcv2alpha1.PodScheduling, list *allowlist.PriorityClassAllowlist) error {
	if s == nil {
		return nil
	}
	name := s.PriorityClassName
	if list.AllowedPodPriorityClass(name) {
		return nil
	}
	return fmt.Errorf(
		"spec.scheduling.priorityClassName: %q is not in the platform infra allowlist %v; "+
			"a PriorityClass sets the scheduler's preemption order across the whole cluster, so the platform admin must "+
			"pre-create it and add it to the GMC --allowed-infra-priority-classes flag "+
			"(kept disjoint from the worker --allowed-priority-classes)",
		name, list.Names())
}

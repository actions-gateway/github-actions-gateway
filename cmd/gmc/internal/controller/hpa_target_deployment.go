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

package controller

import (
	appsv1 "k8s.io/api/apps/v1"
)

// assignHPATargetDeploymentSpec writes the controller-managed fields of an
// HPA-targeted Deployment's spec onto live, leaving `.spec.replicas` to the
// HorizontalPodAutoscaler that names this Deployment in its scaleTargetRef.
//
// A blanket `live = desired` reverts every HPA scale-out (Q283): the builders
// stamp minReplicas as the desired replica count, and because both proxy
// reconcilers `Owns(&appsv1.Deployment{})`, the HPA's own write to the scale
// subresource requeues a reconcile that immediately puts the count back. The
// pool then oscillates between minReplicas and the HPA's target and can never
// stay scaled out — capping the egress capacity of every tenant whose only path
// to GitHub runs through the proxy pool.
//
// The division of labour is: the reconciler owns the *floor*, the HPA owns
// everything above it.
//
//   - Create (live.Replicas == nil): the floor is the initial replica count.
//   - live.Replicas == 0: restore the floor. An HPA refuses to act on a target
//     sitting at zero replicas ("scaling is disabled since the replica count of
//     the target is zero" — HPAScaleToZero is alpha and off by default), so
//     nothing else would ever bring the pool back and the tenant's egress path
//     would stay down for good.
//   - Any other live count: left untouched. Scale-down below the floor is
//     prevented by the HPA's own `spec.minReplicas`, which the reconciler does
//     keep in sync with the CR.
//
// Every field the builders do set is assigned individually, mirroring the
// applyService helpers: the server-defaulted spec fields (strategy,
// revisionHistoryLimit, progressDeadlineSeconds, minReadySeconds) are left as
// the apiserver stored them. A new builder-set DeploymentSpec field must be
// added here, or it will silently never be reconciled.
func assignHPATargetDeploymentSpec(live *appsv1.DeploymentSpec, desired appsv1.DeploymentSpec) {
	live.Selector = desired.Selector
	live.Template = desired.Template
	if live.Replicas == nil || *live.Replicas == 0 {
		live.Replicas = desired.Replicas
	}
}

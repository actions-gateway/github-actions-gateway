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

package v2beta1

// The v2beta1 kinds are the conversion **hub** for the actions-gateway.com group
// (Q74): each implements sigs.k8s.io/controller-runtime/pkg/conversion.Hub by
// carrying a no-op Hub() marker. v2alpha1 is the Convertible spoke — it alone
// carries ConvertTo/ConvertFrom (see api/v2alpha1/conversion.go). Hub-and-spoke
// keeps the conversion count linear: every served version converts to/from this one
// hub rather than to every other version pairwise.
//
// v2beta1 is also the storage version (+kubebuilder:storageversion on each root
// kind), so a persisted object is already in hub shape and the apiserver only
// invokes the webhook to serve a v2alpha1 read or admit a v2alpha1 write.

// Hub marks ActionsGateway as the conversion hub for its kind.
func (*ActionsGateway) Hub() {}

// Hub marks EgressProxy as the conversion hub for its kind.
func (*EgressProxy) Hub() {}

// Hub marks RunnerSet as the conversion hub for its kind.
func (*RunnerSet) Hub() {}

// Hub marks RunnerTemplate as the conversion hub for its kind.
func (*RunnerTemplate) Hub() {}

// Hub marks ClusterRunnerTemplate as the conversion hub for its kind.
func (*ClusterRunnerTemplate) Hub() {}

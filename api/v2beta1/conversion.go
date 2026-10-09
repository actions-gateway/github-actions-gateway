package v2beta1

// The v2beta1 kinds are the conversion **hub** for the actions-gateway.com group
// (Q74): each implements sigs.k8s.io/controller-runtime/pkg/conversion.Hub by
// carrying a no-op Hub() marker. v2alpha1 and v2 are the Convertible spokes — they
// alone carry ConvertTo/ConvertFrom (see api/v2alpha1/conversion.go and
// api/v2/conversion.go). Hub-and-spoke keeps the conversion count linear: every
// served version converts to/from this one hub rather than to every other version
// pairwise. api/v2/conversion.go says why the hub stays here rather than at v2.
//
// The storage version is v2 from 1.10 (Q1086), not the hub, so the apiserver
// invokes the webhook for every v2alpha1 or v2beta1 read and write; a v2 request
// needs no conversion.

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

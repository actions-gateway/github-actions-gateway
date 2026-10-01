package v2

import (
	agcv2 "github.com/actions-gateway/github-actions-gateway/api/v2"
)

// The validators are typed on v2, so a write at v2alpha1 or v2beta1 reaches them
// converted through the v2beta1 hub. Two values survive that conversion only as
// annotations, because v2 has no field for them; these readers rebuild them. The keys
// are private to api/v2 and api/v2alpha1, so they are repeated here and
// TestConversionAnnotations_SurviveTheHub pins them to the real conversions.
const (
	// annEgressPolicyMode carries a stored CiliumFQDN/CalicoFQDN alias on the v2
	// view, which shows FQDN (api/v2 conversion.go).
	annEgressPolicyMode = "conversion.actions-gateway.com/egress-policy-mode"
	// annAcquisitionProtocol carries a v2alpha1 RunnerSet's acquisitionProtocol
	// through the ScaleSet-only hub (api/v2alpha1 conversion.go).
	annAcquisitionProtocol = "conversion.actions-gateway.com/acquisition-protocol"

	egressPolicyModeCiliumFQDN agcv2.EgressPolicyMode = "CiliumFQDN"
	egressPolicyModeCalicoFQDN agcv2.EgressPolicyMode = "CalicoFQDN"

	acquisitionProtocolScaleSet = "ScaleSet"
)

// egressPolicyModeOf returns the mode ep stores: its deprecated alias when the v2 view
// carries one, else spec.egressPolicyMode. It keeps an alias only while the mode is
// FQDN, as api/v2's ConvertTo does, so a v2 write that sets another mode or drops the
// annotation reads as the migration it is.
func egressPolicyModeOf(ep *agcv2.EgressProxy) agcv2.EgressPolicyMode {
	mode := ep.Spec.EgressPolicyMode
	if mode != agcv2.EgressPolicyModeFQDN {
		return mode
	}
	switch alias := agcv2.EgressPolicyMode(ep.Annotations[annEgressPolicyMode]); alias {
	case egressPolicyModeCiliumFQDN, egressPolicyModeCalicoFQDN:
		return alias
	}
	return mode
}

// acquisitionProtocolOf returns rs's acquisition protocol: the v2alpha1 value the
// conversion carried, else ScaleSet, which is what a set written at v2beta1 or v2 is.
func acquisitionProtocolOf(rs *agcv2.RunnerSet) string {
	if p, ok := rs.Annotations[annAcquisitionProtocol]; ok {
		return p
	}
	return acquisitionProtocolScaleSet
}

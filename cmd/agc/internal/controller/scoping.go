package controller

// ServesRunnerGroups reports whether an AGC carrying this GATEWAY_NAME reconciles
// actions-gateway.github.com/v1alpha1 RunnerGroups. Only the v1 AGC does: a
// RunnerGroup names no gateway — v1 is a namespace singleton — so the AGC that serves
// it is the one the v1 GMC provisioned, and that one stamps no GATEWAY_NAME.
//
// A gateway-scoped AGC reconciling RunnerGroups too ran a second listener pool at the
// same agentIndex as the v1 AGC's — same agent Secret, same GitHub runner name — which
// took 409 from the broker on every CreateSession (153 in ~2.5 minutes, no backoff)
// while both reconcilers wrote the same RunnerGroup status (Q535, measured live on the
// dogfood cluster). Nothing can scope a RunnerGroup informer the way
// spec.gatewayRef.name scopes the RunnerSet one, so declining the kind is the fix — the
// mirror of the gate Q466 put on the RunnerSet side.
func ServesRunnerGroups(gatewayName string) bool { return gatewayName == "" }

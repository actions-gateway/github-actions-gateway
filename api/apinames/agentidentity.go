package apinames

import "strconv"

// The agent-identity stem, shared by the AGC that derives names from it and the GMC
// admission that rejects two owners deriving the same one (Q1011).
//
// An agent pool derives two names per index from its owner CR: the agent Secret
// "agentpool-<stem>-<index>", namespaced with the CR, and the runner name
// "<stem>-<index>" registered with GitHub, which is unique per org, enterprise, or
// repo. Q466 keeps a v1alpha1 RunnerGroup and a v2 RunnerSet of the same name apart
// by giving the RunnerSet stem an "rs-" prefix. That discriminator is NOT injective:
// a RunnerGroup named "rs-<x>" derives exactly the stem a RunnerSet named "<x>"
// does, so both pools claim one Secret and one GitHub runner record, each
// deregistering the other's — and neither can self-heal (agentpool.ErrAgentNameCollision).
//
// The derivation lives here rather than in the AGC pool for the reason this package
// exists at all: two layers deriving one name from the same CR must not hold
// separate copies of the rule. The GMC's admission guard compares stems it derives
// from CR names it has never seen materialise, so a divergence would make it reject
// pairs that do not collide and admit pairs that do.

// RunnerSetStemPrefix discriminates a v2 RunnerSet's agent identity from a v1alpha1
// RunnerGroup's (Q466). It is a plain prefix and therefore not injective across the
// two name spaces; see [RunnerSetAgentStem].
const RunnerSetStemPrefix = "rs-"

// RunnerGroupAgentStem returns the agent-identity stem a v1alpha1 RunnerGroup named
// name derives its agent Secret and GitHub runner names from. The RunnerGroup is the
// original derivation, so its stem is the CR name unchanged.
func RunnerGroupAgentStem(name string) string {
	return name
}

// RunnerSetAgentStem returns the agent-identity stem a v2 RunnerSet named name
// derives its agent Secret and GitHub runner names from: the CR name under
// [RunnerSetStemPrefix].
func RunnerSetAgentStem(name string) string {
	return RunnerSetStemPrefix + name
}

// RunnerGroupName derives the v1alpha1 RunnerGroup CR name the GMC materializes for
// one ActionsGateway spec.runnerGroups[index] entry: the gateway name joined to a
// sanitised form of the entry's FIRST runner label, or to the entry's index when the
// entry carries no labels at all.
//
// It lives here because four callers must agree on it exactly and none of them
// materializes the object the others read: the GMC controller creates the CR,
// gag-migrate synthesizes the same name so its standalone-vs-inline dedup is exact,
// GMC admission derives it to reject a name that would claim a taken agent-identity
// stem (Q1011), and the AGC stamps it as a label value on every worker pod and agent
// Secret.
//
// The [MaxLabelValue] budget is that last consumer's, NOT the 253-character limit on
// a CR name: an unbounded name is accepted at create and rejected wherever it is
// used as a label value. A 15-character gateway with a 40-character runner label was
// enough to overrun it, after which every worker pod create failed and the tenant ran
// no jobs while GitHub reported only that the runner had lost communication. v2 caps
// CR names at 52 characters in CEL ([§H.6]); v1 has no such cap, so the bound is
// applied where the name is derived. [Join] returns a name that already fits
// unchanged, so only a tenant that is already broken is renamed.
//
// [§H.6]: https://github.com/actions-gateway/github-actions-gateway/blob/main/docs/design/appendix-h-v2-api-decomposition.md#h6-naming-and-length-budgets
func RunnerGroupName(gatewayName string, runnerLabels []string, index int) string {
	if len(runnerLabels) > 0 {
		return Join(MaxLabelValue, gatewayName, Segment(runnerLabels[0], "label"))
	}
	return Join(MaxLabelValue, gatewayName, strconv.Itoa(index))
}

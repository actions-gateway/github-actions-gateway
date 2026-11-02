package apinames

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

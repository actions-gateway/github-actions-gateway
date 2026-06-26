# Admission-policy samples (Kyverno / Gatekeeper)

Complementary policy samples for running github-actions-gateway (GAG) alongside
a cluster policy engine. Full context, the compatibility matrix, and apply
instructions are in
[../../admission-policies.md](../../admission-policies.md).

| File | Engine | Purpose |
|---|---|---|
| [`kyverno/enforce-gag-worker-hardening.yaml`](kyverno/enforce-gag-worker-hardening.yaml) | Kyverno | Enforce GAG's own posture in tenant namespaces. |
| [`kyverno/policyexception-gag.yaml`](kyverno/policyexception-gag.yaml) | Kyverno | Exempt GAG pods from strict cluster rules they cannot satisfy. |
| [`gatekeeper/enforce-gag-worker-hardening.yaml`](gatekeeper/enforce-gag-worker-hardening.yaml) | Gatekeeper | `ConstraintTemplate` + `Constraint` enforcing GAG's posture. |
| [`gatekeeper/exclude-gag-namespaces.yaml`](gatekeeper/exclude-gag-namespaces.yaml) | Gatekeeper | Exclude GAG namespaces from strict cluster `Constraint`s. |

Each file is commented with what it does, what to edit for your cluster, and how
to roll it out safely (audit/dry-run first). Treat them as starting points —
adapt the policy names and install-namespace references to your environment.

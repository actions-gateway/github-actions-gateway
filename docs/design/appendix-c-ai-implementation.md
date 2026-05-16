# Appendix C — AI-Assisted Implementation Notes (Optional)

← [Appendix B](appendix-b-worker-isolation.md) | [Back to index](README.md) | Next: [Appendix D — Alternatives Considered →](appendix-d-alternatives-considered.md)

---

The milestones in [§6](06-implementation-phases.md) describe *what* to build and *how to verify* it; they do not prescribe a working style. This appendix collects guidance for teams who choose to leverage AI-assisted coding tools (e.g. Claude Code, GitHub Copilot, Cursor) during implementation. The guidance is optional and orthogonal to the design itself.

---

## C.1. Comparative Advantages

AI-assisted implementation is well-suited to this design for several reasons:

* **Boilerplate-heavy scaffolding.** Both controllers are `controller-runtime`/`kubebuilder` operators with deeply conventional reconcile loops, RBAC markers, status update patterns, and CRD type definitions. AI tools generate this skeleton in minutes with high accuracy, freeing engineering time for the parts that actually require judgment.
* **Table-driven tests.** Sections [7.1](07-test-plan.md#71-unit-tests) and [7.2](07-test-plan.md#72-integration-tests) specify dozens of test cases following uniform table-driven patterns. AI tools are particularly strong at generating these from a description of the inputs, expected outputs, and edge cases.
* **Repetitive resource generation.** The GMC's reconciler emits ten kinds of Kubernetes objects per tenant; assembling each `corev1.Service`, `appsv1.Deployment`, `autoscalingv2.HorizontalPodAutoscaler`, etc. is exactly the kind of structured code generation AI tools handle well.
* **Fixture-based payload work.** The decrypted job payload from Milestone 1 is large and opaque. AI tools can derive Go struct definitions, parse helpers, and field-validation tests from the fixture without needing a separate schema spec.
* **Shifting the engineer's role to review.** The five-week timeline assumes the engineer spends more time reviewing, integrating, and validating than typing. AI tools enable this redistribution by absorbing the typing.

---

## C.2. Risks and Mitigations

AI-assisted implementation introduces specific failure modes that are worth naming explicitly.

* **Plausible but wrong code.** AI tools confidently produce code that compiles and reads correctly but is wrong in subtle ways — calling the wrong endpoint, conflating `broker_url` and `run_service_url`, treating a Secret as mutable when it isn't, or skipping a critical `defer` on a mutex. **Mitigation:** every AI-generated PR is reviewed by an engineer who has read the relevant section of this design doc; the design doc, not the AI tool, is the source of truth.
* **Hidden security regressions.** AI tools may generate ClusterRoles with overly broad verbs (`*` on `secrets`, wildcard resource selectors) because that is the path of least resistance. **Mitigation:** the [§7.1](07-test-plan.md#71-unit-tests) "GMC RBAC boundary assertions" test exists specifically to catch this; run it on every PR and fail the build on violations.
* **Drift from the design doc.** AI tools optimize for solving the immediate prompt and will happily diverge from the documented API contracts if not corrected. **Mitigation:** feed the specific design-doc section into the prompt for each task, not just the high-level goal; cross-reference generated CRD field names against [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas) before merging.
* **Test coverage that mirrors the code.** AI tools generating tests after the implementation tend to test what the code *does*, not what it *should* do. **Mitigation:** write the test specification (the bullet under [§7](07-test-plan.md)) before asking the AI to implement either the code or the test. Treat the design doc's test list as the contract.
* **Erosion of mental model.** Engineers who rely heavily on AI for implementation may end up with a shallower understanding of the resulting system, slowing later debugging. **Mitigation:** the engineer is expected to be able to explain every merged PR without referring back to the AI conversation. If they cannot, the PR is not ready to merge.

---

## C.3. Recommended Prompting Structure Per Milestone

These prompts are *suggestions*; adapt to taste. Each milestone references the design-doc section that should be pasted into the prompt as ground truth.

* **[Milestone 1](06-implementation-phases.md#milestone-1-wire-protocol-probe-days-14) (Wire Protocol Probe).** Feed the broker endpoint table ([§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints)) and payload structs ([§3.4](03-api-contracts.md#34-broker-payload-blueprints-go-structs)) as the source of truth. Ask for a standalone `cmd/probe/main.go` that authenticates via GitHub App credentials, calls `POST /sessions`, long-polls `GET /message`, calls `POST /acquirejob` on the `run_service_url` from the message body, and starts a `renewjob` loop. Be explicit about the two-URL pitfall called out in [§3.3](03-api-contracts.md#33-re-implemented-broker-api-endpoints) — ask for the distinction to appear in named variables, not assumed.
* **[Milestone 2](06-implementation-phases.md#milestone-2-agc-controller--reconciler-days-510) (AGC).** Ask for `kubebuilder`-style scaffolding from the `RunnerGroup` CRD spec in [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas). Iterate on the goroutine session registry, Token Manager (mutex-protected token with T-5min proactive refresh), and per-job RenewJob loop. Use the Milestone 1 probe as the polling implementation. Pair every generated function with a table-driven unit test and a `goleak.VerifyNone(t)` assertion in test teardown.
* **[Milestone 3](06-implementation-phases.md#milestone-3-worker-pod--pipe-handoff-days-1116) (Worker pod).** This is the riskiest phase for AI-assisted work because the Named Pipe handoff is underdocumented in the upstream `actions/runner` codebase. Validate the entrypoint wrapper against the static decrypted payload from Milestone 1 *before* wiring it into pod creation, so the pipe semantics can be debugged in isolation.
* **[Milestone 4](06-implementation-phases.md#milestone-4-gateway-manager-controller--proxy-days-1722) (GMC + Proxy).** Ask for a second `cmd/` entry point in the same repo, sharing CRD types. Feed the `ActionsGateway` CRD spec from [§3.1](03-api-contracts.md#31-kubernetes-crd-schemas) verbatim. Pay close attention to generated RBAC — write the [§7.1](07-test-plan.md#71-unit-tests) "GMC RBAC boundary assertions" test *first*, before merging the GMC code. The CONNECT proxy itself is a small artifact (~150 lines) and an excellent target for AI generation.
* **[Milestone 5](06-implementation-phases.md#milestone-5-hardening--load-testing-days-2326) (Hardening & Load).** Ask for NetworkPolicy manifests scoped to the proxy label, PodSecurityContext hardening for both proxy and AGC, and per-namespace ResourceQuota definitions derived from [§5](05-security.md) and [Appendix A](appendix-a-capacity-slos.md). Extend the load harness from Milestone 1 to multiple tenants. **Review all generated security manifests by hand before applying to any real cluster** — this is the highest-stakes review pass of the entire project.

---

## C.4. When AI-Assisted Implementation Is the Wrong Choice

If your team has not used these tools before, the five-week timeline may not include enough slack to learn the tools and the codebase simultaneously. Conventional implementation with a longer timeline is a safe alternative; the design doc is identical and the milestones still apply.

---

← [Appendix B](appendix-b-worker-isolation.md) | [Back to index](README.md) | Next: [Appendix D — Alternatives Considered →](appendix-d-alternatives-considered.md)

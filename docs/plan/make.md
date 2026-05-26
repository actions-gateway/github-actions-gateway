# Makefile UX Plan

## Status at a glance

Last refreshed 2026-05-25. Phase 1 (day-one UX) has fully landed. Phase 2
(consistency cleanup) is partially done — the image-name and envtest
drift between root and `cmd/gmc/Makefile` remain.

| # | Item | File | Status |
|---|---|---|---|
| 1.1 | `make help` + `.DEFAULT_GOAL := help`, `##@` sections | [Makefile:31,38,41](../../Makefile) | ✅ Done |
| 1.2 | `.PHONY` includes all targets | [Makefile](../../Makefile) | ✅ Done — `e2e-load-images` consolidated into `e2e-images`; `.PHONY` block updated |
| 1.3 | Stop swallowing `kind` errors | [Makefile](../../Makefile) | ✅ Done — no `\|\| true` remains in cluster targets |
| 1.4 | `e2e-up` umbrella target | [Makefile](../../Makefile) | ✅ Done — `e2e-up: e2e-cluster e2e-images e2e` |
| 1.5 | `KIND_CONFIG` default | [Makefile](../../Makefile) | ✅ Done — now `test/kind-config-2worker.yaml` |
| 2.1 | Unify image variable names across Makefiles | [Makefile](../../Makefile), [cmd/gmc/Makefile](../../cmd/gmc/Makefile) | ❌ Open — root uses `*_IMG`, GMC uses `IMG`/`AGC_IMAGE`/`PROXY_IMAGE` |
| 2.2 | Consistent SHA-based image tagging | [Makefile](../../Makefile) | ✅ Done — all four images use `:e2e-$(GIT_SHA)` |
| 2.3 | Single source of truth for `setup-envtest` | [cmd/gmc/Makefile](../../cmd/gmc/Makefile) | ❌ Open — GMC still has its own `go install ...@$(ENVTEST_VERSION)` path alongside root's `$(SETUP_ENVTEST)` |
| 2.4 | DRY ginkgo invocations | [Makefile:141-144](../../Makefile) | ✅ Done — single `e2e` target with `SUITE=` selector replaces the three-target duplication |
| 2.5 | Consistent build invocation style (`go -C` vs `cd &&`) | various | ⓘ Minor — no follow-up needed unless someone touches the file again |
| 2.6 | Align `all` semantics across Makefiles | [Makefile](../../Makefile), [cmd/agc/Makefile](../../cmd/agc/Makefile), [cmd/gmc/Makefile](../../cmd/gmc/Makefile) | ❌ Open — root `all: build`; agc/gmc `all: generate build test` |
| 2.7a | `e2e-clean` actually cleans (images + `.build/`) | [Makefile](../../Makefile) | ❌ Open — still a one-line alias for `e2e-cluster-delete` |
| 2.7b | `make tools` prints progress | [Makefile](../../Makefile) | ⓘ Cosmetic — defer unless someone reports it |

### Open work (priority order)

1. **2.1 + 2.3** — Unify image var names *and* delete the GMC's
   separate envtest install. These are the two real drift items between
   the root and per-binary Makefiles. Cheap, related, land together.
2. **2.6** — Align `all` semantics. One-line fix.
3. **2.7a** — Make `e2e-clean` actually clean. Small.
4. **2.5 / 2.7b** — Cosmetic; defer until next Makefile touch.

---

## Current state

The repo has three Makefiles — the root [Makefile](../../Makefile) plus per-binary Makefiles at [cmd/gmc/Makefile](../../cmd/gmc/Makefile) and [cmd/agc/Makefile](../../cmd/agc/Makefile). They work, but discoverability is poor (no `help` target, ~14 root targets that don't appear in the README), the local e2e workflow is a four-step ordered sequence with no umbrella target, and several details have drifted between the three files (image variable names, envtest install path, `all` semantics). This plan describes the changes worth making in two phases: a "day-one contributor" pass that lands the user-visible UX wins, followed by a cleanup pass that removes inconsistencies between Makefiles.

The goal is that a new contributor can clone the repo, type `make`, and learn what is available; and that running the e2e suite locally takes one command, not four.

---

## Phase 1 — Day-one contributor UX

These changes are what a new contributor would feel on their first afternoon. None of them are architectural; they are visible quality-of-life improvements.

### 1.1 Add `make help` and make it the default goal

Add a self-documenting `help` target to the root [Makefile](../../Makefile) (and to [cmd/gmc/Makefile](../../cmd/gmc/Makefile) and [cmd/agc/Makefile](../../cmd/agc/Makefile)) that parses `## ` comments after the target name and prints them as a grouped table. Set `.DEFAULT_GOAL := help` so a bare `make` prints the menu instead of starting a build.

Annotate every public target with a one-line `##` description. Group with section headers (`##@ Build`, `##@ e2e`, `##@ Tools`) so the output is browsable. The README's Development section should be updated to direct users to `make help` rather than enumerating a subset of targets.

### 1.2 Fix the `.PHONY` declaration

The root [Makefile:21‑24](../../Makefile:21) lists most targets but omits `e2e-load-images`. Add it. While there, consider switching to one `.PHONY:` line per target (the style used in [cmd/agc/Makefile](../../cmd/agc/Makefile)) — it makes future additions less error-prone than maintaining a single multi-line block.

### 1.3 Stop swallowing `kind` errors

[Makefile:40](../../Makefile:40) and [Makefile:44](../../Makefile:44) use `|| true` to make `e2e-cluster` and `e2e-cluster-delete` idempotent, but they also hide "kind not installed", "Docker daemon not running", and every other real error. Replace with a conditional:

```
e2e-cluster:
	@if kind get clusters | grep -qx $(KIND_CLUSTER); then \
		echo "==> kind cluster $(KIND_CLUSTER) already exists"; \
	else \
		kind create cluster --name $(KIND_CLUSTER) --config $(KIND_CONFIG); \
	fi
```

Apply the inverse for delete.

### 1.4 Chain the e2e workflow into a single target

Today the happy path is:

```
make e2e-cluster
make e2e-images
make e2e-load-images
make e2e
```

Make `e2e-load-images` depend on `e2e-images`, and add an umbrella `e2e-up` target that ensures the cluster exists and runs the full sequence:

```
e2e-up: e2e-cluster e2e-load-images e2e
```

The standalone targets stay so CI (which builds images differently) is unaffected. The README and `docs/plan/e2e-tests.md` should reference `make e2e-up` as the canonical local command.

### 1.5 Default `KIND_CONFIG` to the 2-node CI config

[Makefile:13‑14](../../Makefile:13) defaults to `test/kind-config.yaml` (3 nodes), but the default `make e2e` target excludes multi-node tests with `--label-filter '!multi-node'` ([Makefile:75](../../Makefile:75)). Local developers pay for an extra worker node they don't use unless they remember the override.

Change the default to `test/kind-config-ci.yaml`. Have `e2e-multi-node` and `e2e-all` override `KIND_CONFIG` to the 3-node config so they continue to work. Document in the file comment that the 3-node config is opt-in via those targets.

---

## Phase 2 — Cleanup and consistency

These are not user-facing but they reduce drift and make the Makefiles easier to maintain.

### 2.1 Unify image variable names

Root [Makefile](../../Makefile) uses `GMC_IMG`, `AGC_IMG`, `PROXY_IMG`, `FAKEGITHUB_IMG`. [cmd/gmc/Makefile](../../cmd/gmc/Makefile) uses `IMG`, `AGC_IMAGE`, `PROXY_IMAGE`. A user running `make e2e-images` followed by `make -C cmd/gmc deploy` has to re-pass the same value with different names.

Pick one set of names (the `*_IMG` short form is shorter and already in the root file) and use them consistently across all three Makefiles. Have the GMC Makefile read defaults from the root names with `?=`.

### 2.2 Consistent SHA-based image tagging

[Makefile:16](../../Makefile:16) tags `GMC_IMG` with `:e2e-$(GIT_SHA)` but the other three images use static `:e2e`. The intent of the SHA tag is to invalidate kind's image cache when code changes — but it only works for GMC. Either tag all four images with the SHA (the right answer; mixed builds are an obscure source of stale-image bugs) or tag none of them. Recommend the former.

### 2.3 Single source of truth for envtest

[Makefile:118‑120](../../Makefile:118) builds `setup-envtest` from the vendored `tools/` module. [cmd/gmc/Makefile:63‑64](../../cmd/gmc/Makefile:63) installs it separately with `go install ...@release-0.23`. These are two version pins that can drift. Delete the `go install` path in the GMC Makefile and depend on `$(SETUP_ENVTEST)` from the root, the same way the GMC Makefile already depends on `$(CONTROLLER_GEN)`.

### 2.4 DRY the ginkgo invocations

The `e2e`, `e2e-multi-node`, and `e2e-all` targets at [Makefile:70‑99](../../Makefile:70) repeat the same env block (`KIND_CLUSTER`, four image vars) and most of the same flags. Factor:

```
GINKGO_E2E_ENV  := KIND_CLUSTER=$(KIND_CLUSTER) GMC_IMG=$(GMC_IMG) AGC_IMG=$(AGC_IMG) PROXY_IMG=$(PROXY_IMG) FAKEGITHUB_IMG=$(FAKEGITHUB_IMG)
GINKGO_E2E_BASE := --tags e2e --timeout 30m --github-output --poll-progress-after 60s
```

Each target then sets only its label filter, `--procs`, and `--junit-report` path.

### 2.5 Consistent build invocation style

Root [Makefile:31,34](../../Makefile:31) uses Go's `-C cmd/agc` flag. [Makefile:71](../../Makefile:71) uses shell `cd cmd/gmc &&`. Pick one. `go build -C` is cleaner for Go invocations; for ginkgo (not a Go subcommand) `cd && ...` is fine — but the inconsistency is worth a comment if both stay.

### 2.6 Align `all` semantics across Makefiles

Root: `all: build`. [cmd/agc/Makefile:7](../../cmd/agc/Makefile:7): `all: generate build test`. [cmd/gmc/Makefile:16](../../cmd/gmc/Makefile:16): `all: generate build test`. A user who runs `make` in different directories gets surprisingly different behavior. Once `help` is the default goal (Phase 1.1), `all` becomes opt-in and can be defined consistently — recommend `all: generate build test` everywhere, or remove `all` entirely and use named targets.

### 2.7 Minor

- `e2e-clean` ([Makefile:102](../../Makefile:102)) is a one-line alias for `e2e-cluster-delete`. Either drop it or make it actually clean — delete the kind cluster *and* remove the built images and `.build/` artifacts (which is what `clean` usually means in a Makefile).
- `make tools` is silent. Borrow the `==> building controller-gen` style from [scripts/setup.sh](../../scripts/setup.sh) so users can see which tool is being built when it takes 30 seconds.

---

## Out of scope

- **Build-time caching of Docker images.** `e2e-images` rebuilds all four images on every invocation even if nothing changed. A sentinel-file approach (touch `.build/img-gmc.stamp` after a successful build, depend the target on the source files) would skip rebuilds, but is enough complexity to deserve its own design pass. Not in this plan.
- **Parallel image builds.** `make -j4 e2e-images` already works for the four `docker-build-*` targets in principle; whether the Docker daemon can keep up is a separate question. No change required.
- **Replacing Make with Task / Just / Mage.** This plan keeps Make. A tool migration is a larger discussion with its own tradeoffs.

---

## Order of work

Phase 1 lands as one PR — five small, independent commits. Each is reversible and adds no new dependencies. The PR description should call out the change in `KIND_CONFIG` default (1.5) since it's the one behavioral change a returning contributor might notice.

Phase 2 lands as a second PR after Phase 1 has been in `main` for a few days. It is pure cleanup and can be split further if review surfaces concerns about any individual change.

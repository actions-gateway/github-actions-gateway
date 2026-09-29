#!/usr/bin/env bash
# ci-identity-setup.sh — give GitHub Actions a keyless identity on the dogfood
# project, so the release-candidate gate can run in CI instead of on a
# maintainer's machine (Q880, milestone 1).
#
# CI has no GCP access today, and a service-account key would contradict the
# no-PEM workload identity this project ships as a feature. So the job proves who
# it is with its own GitHub OIDC token, exchanged through Workload Identity
# Federation for a short-lived token of one service account. Nothing long-lived
# is stored anywhere.
#
# The trust boundary is a GitHub environment, not a branch or a tag:
#   * the provider accepts only this repository's tokens (matched by numeric id,
#     which survives a rename) that carry the `environment` claim below;
#   * only that environment's principals may impersonate the service account;
#   * the environment admits only `main` and `v*-rc.*` tags, and every job
#     entering it waits for a named reviewer.
# A fork, a pull request, or any job outside the environment gets no token.
#
# Grants follow the milestone, not the gate. Milestone 1 grants ROLES (default
# roles/container.viewer) — enough for .github/workflows/dogfood-identity-probe.yml
# to read the cluster and to prove it cannot write to it. The gate's own roles
# land with milestone 3 (Q880).
#
# Usage:
#   PROJECT=… CLUSTER=… ZONE=… REPO=owner/name REVIEWERS=login[,login…] \
#     scripts/dogfood/ci-identity-setup.sh
#
# Optional env vars:
#   ENVIRONMENT  GitHub environment name (default dogfood-validation).
#   POOL         Workload identity pool id (default github-actions).
#   PROVIDER     Pool provider id (default github-oidc).
#   SA_NAME      Service account id (default gag-release-validator).
#   ROLES        Space-separated project roles for the service account
#                (default roles/container.viewer).
#   ASSUME_YES=1 Skip the one interactive confirmation.
#
# Idempotent: every create is guarded by a describe, the provider's condition is
# converged when it drifts, and IAM bindings and environment settings are
# declarative. It removes nothing — a role dropped from ROLES stays bound until
# removed by hand.
#
# Exit 0 converged, 1 a step failed, 2 usage.
set -euo pipefail
shopt -s inherit_errexit

REPO_ROOT="$(git rev-parse --show-toplevel)"
# shellcheck source=scripts/lib/common.sh
source "${REPO_ROOT}/scripts/lib/common.sh"

ENVIRONMENT="${ENVIRONMENT:-dogfood-validation}"
POOL="${POOL:-github-actions}"
PROVIDER="${PROVIDER:-github-oidc}"
SA_NAME="${SA_NAME:-gag-release-validator}"
ROLES="${ROLES:-roles/container.viewer}"
ISSUER="https://token.actions.githubusercontent.com"

# The refs the environment admits: `main` for the probe, candidate tags for the gate.
BRANCH_POLICY="main"
TAG_POLICY="v*-rc.*"

usage() {
	sed -n '2,/^set -euo/p' "$0" | sed '$d' | sed 's/^# \{0,1\}//' >&2
	exit 2
}

# attribute_condition REPO_ID OWNER_ID — the CEL the provider evaluates on every
# token exchange. Ids rather than names, so a renamed or re-created repository
# with the same slug is a different principal.
attribute_condition() {
	printf "assertion.repository_id == '%s' && assertion.repository_owner_id == '%s' && assertion.environment == '%s'" \
		"$1" "$2" "${ENVIRONMENT}"
}

ensure_pool() {
	if gcloud iam workload-identity-pools describe "${POOL}" \
		--project="${PROJECT}" --location=global >/dev/null 2>&1; then
		echo "Pool ${POOL} already exists."
		return
	fi
	echo "Creating workload identity pool ${POOL}..."
	gcloud iam workload-identity-pools create "${POOL}" \
		--project="${PROJECT}" --location=global \
		--display-name="GitHub Actions"
}

ensure_provider() {
	local want="$1" have mapping
	mapping="google.subject=assertion.sub,attribute.repository_id=assertion.repository_id,attribute.environment=assertion.environment"
	if have="$(gcloud iam workload-identity-pools providers describe "${PROVIDER}" \
		--project="${PROJECT}" --location=global --workload-identity-pool="${POOL}" \
		--format='value(attributeCondition)' 2>/dev/null)"; then
		if [[ "${have}" == "${want}" ]]; then
			echo "Provider ${PROVIDER} already exists with the expected condition."
			return
		fi
		echo "Provider ${PROVIDER} condition drifted; converging it."
		echo "  have: ${have}"
		echo "  want: ${want}"
		gcloud iam workload-identity-pools providers update-oidc "${PROVIDER}" \
			--project="${PROJECT}" --location=global --workload-identity-pool="${POOL}" \
			--issuer-uri="${ISSUER}" \
			--attribute-mapping="${mapping}" \
			--attribute-condition="${want}"
		return
	fi
	echo "Creating OIDC provider ${PROVIDER}..."
	gcloud iam workload-identity-pools providers create-oidc "${PROVIDER}" \
		--project="${PROJECT}" --location=global --workload-identity-pool="${POOL}" \
		--display-name="GitHub Actions OIDC" \
		--issuer-uri="${ISSUER}" \
		--attribute-mapping="${mapping}" \
		--attribute-condition="${want}"
}

ensure_service_account() {
	local email="$1"
	if gcloud iam service-accounts describe "${email}" --project="${PROJECT}" >/dev/null 2>&1; then
		echo "Service account ${email} already exists."
		return
	fi
	echo "Creating service account ${email}..."
	gcloud iam service-accounts create "${SA_NAME}" --project="${PROJECT}" \
		--display-name="GAG release-candidate validator (GitHub Actions)"
}

bind_iam() {
	local email="$1" number="$2" role
	echo "Letting ${ENVIRONMENT} jobs impersonate ${email}..."
	gcloud iam service-accounts add-iam-policy-binding "${email}" \
		--project="${PROJECT}" \
		--role=roles/iam.workloadIdentityUser \
		--member="principalSet://iam.googleapis.com/projects/${number}/locations/global/workloadIdentityPools/${POOL}/attribute.environment/${ENVIRONMENT}" \
		>/dev/null
	for role in ${ROLES}; do
		echo "Granting ${role} on ${PROJECT}..."
		gcloud projects add-iam-policy-binding "${PROJECT}" \
			--member="serviceAccount:${email}" --role="${role}" --condition=None \
			>/dev/null
	done
}

# ensure_environment — required reviewers plus a custom ref policy. PUT is a full
# replace of the protection rules, so it converges on every run; the ref policies
# are separate resources and are only added when missing.
ensure_environment() {
	local reviewers_json="$1" have
	echo "Configuring environment ${ENVIRONMENT} on ${REPO}..."
	gh api -X PUT "repos/${REPO}/environments/${ENVIRONMENT}" --input - >/dev/null <<-EOF
		{
		  "reviewers": ${reviewers_json},
		  "prevent_self_review": false,
		  "deployment_branch_policy": {"protected_branches": false, "custom_branch_policies": true}
		}
	EOF
	have="$(gh api "repos/${REPO}/environments/${ENVIRONMENT}/deployment-branch-policies" \
		--jq '.branch_policies[] | .type + " " + .name')"
	add_ref_policy "${have}" branch "${BRANCH_POLICY}"
	add_ref_policy "${have}" tag "${TAG_POLICY}"
}

add_ref_policy() {
	local have="$1" type="$2" name="$3"
	if grep -qxF "${type} ${name}" <<<"${have}"; then
		echo "  ${type} policy ${name} already present."
		return
	fi
	echo "  adding ${type} policy ${name}"
	gh api -X POST "repos/${REPO}/environments/${ENVIRONMENT}/deployment-branch-policies" \
		-f "name=${name}" -f "type=${type}" >/dev/null
}

set_variables() {
	local provider_name="$1" email="$2" name value
	echo "Setting ${ENVIRONMENT} variables (none are secrets)..."
	while read -r name value; do
		gh variable set "${name}" --env "${ENVIRONMENT}" --repo "${REPO}" --body "${value}"
	done <<-EOF
		GCP_WORKLOAD_IDENTITY_PROVIDER ${provider_name}
		GCP_SERVICE_ACCOUNT ${email}
		DOGFOOD_PROJECT ${PROJECT}
		DOGFOOD_CLUSTER ${CLUSTER}
		DOGFOOD_ZONE ${ZONE}
	EOF
}

main() {
	[[ "${1:-}" == -h || "${1:-}" == --help ]] && usage
	[[ $# -eq 0 ]] || usage
	: "${PROJECT:?PROJECT must be set}"
	: "${CLUSTER:?CLUSTER must be set}"
	: "${ZONE:?ZONE must be set}"
	: "${REPO:?REPO must be set (owner/name)}"
	: "${REVIEWERS:?REVIEWERS must be set (comma-separated GitHub logins)}"

	require_cmd gcloud "https://cloud.google.com/sdk/docs/install"
	require_cmd gh "https://cli.github.com/"

	local number repo_ids repo_id owner_id login uid reviewers_json="" condition email provider_name
	number="$(gcloud projects describe "${PROJECT}" --format='value(projectNumber)')"
	[[ "${number}" =~ ^[0-9]+$ ]] || die "could not read the project number of ${PROJECT} (got '${number}')"

	repo_ids="$(gh api "repos/${REPO}" --jq '"\(.id) \(.owner.id)"')"
	read -r repo_id owner_id <<<"${repo_ids}"
	[[ "${repo_id}" =~ ^[0-9]+$ && "${owner_id}" =~ ^[0-9]+$ ]] ||
		die "could not read the numeric ids of ${REPO} (got '${repo_ids}')"

	local -a logins
	IFS=, read -r -a logins <<<"${REVIEWERS}"
	for login in "${logins[@]}"; do
		uid="$(gh api "users/${login}" --jq .id)"
		[[ "${uid}" =~ ^[0-9]+$ ]] || die "could not resolve reviewer ${login} (got '${uid}')"
		reviewers_json+="${reviewers_json:+,}{\"type\":\"User\",\"id\":${uid}}"
	done
	reviewers_json="[${reviewers_json}]"

	condition="$(attribute_condition "${repo_id}" "${owner_id}")"
	email="${SA_NAME}@${PROJECT}.iam.gserviceaccount.com"
	provider_name="projects/${number}/locations/global/workloadIdentityPools/${POOL}/providers/${PROVIDER}"

	confirm_or_exit "$(printf 'About to configure keyless CI access to the dogfood project:\n  Project:     %s (%s)\n  Repo:        %s (id %s, owner id %s)\n  Environment: %s — reviewers %s, refs %s and tags %s\n  Pool:        %s / provider %s\n  SA:          %s\n  Roles:       %s\nThis writes IAM on the project and protection rules on the repository.' \
		"${PROJECT}" "${number}" "${REPO}" "${repo_id}" "${owner_id}" \
		"${ENVIRONMENT}" "${REVIEWERS}" "${BRANCH_POLICY}" "${TAG_POLICY}" \
		"${POOL}" "${PROVIDER}" "${email}" "${ROLES}")"

	step "Enabling the token-exchange APIs"
	gcloud services enable iam.googleapis.com iamcredentials.googleapis.com sts.googleapis.com \
		--project="${PROJECT}"

	step "Workload identity pool and provider"
	ensure_pool
	ensure_provider "${condition}"

	step "Service account and bindings"
	ensure_service_account "${email}"
	bind_iam "${email}" "${number}"

	step "GitHub environment"
	ensure_environment "${reviewers_json}"
	set_variables "${provider_name}" "${email}"

	echo
	echo "Done. Prove it end to end (it waits for a reviewer's approval):"
	echo "  gh workflow run dogfood-identity-probe.yml --repo ${REPO} --ref main"
}

if [[ "${CI_IDENTITY_LIB_ONLY:-}" != 1 ]]; then
	main "$@"
fi

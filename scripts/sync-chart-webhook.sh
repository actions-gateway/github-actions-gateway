#!/usr/bin/env bash
#
# Single source of truth for the Helm chart's validating-webhook template (Q143).
#
# The chart ships the ValidatingWebhookConfiguration under
# charts/actions-gateway/templates/webhook.yaml, but the authoritative webhook
# *body* (rules, failurePolicy, admissionReviewVersions, sideEffects, the service
# path) is the controller-gen output of the +kubebuilder:webhook marker, emitted
# to cmd/gmc/config/webhook/manifests.yaml (the same file the GMC integration
# suite loads into envtest). Hand-copying that body into the chart invites silent
# drift: a marker change — a new intercepted resource/operation, a path or
# failurePolicy change — that is regenerated into config/ but not propagated to
# the chart copy would leave the deployed admission webhook out of step with the
# code. This script regenerates the chart template FROM the controller-gen
# source, re-injecting the Helm wiring the chart needs (name prefix + labels, the
# cert-manager CA-inject annotation, the templated namespace, and the
# non-cert-manager caBundle block) so the body stays single-sourced.
#
#   scripts/sync-chart-webhook.sh            # write the chart webhook template (make chart-webhook)
#   scripts/sync-chart-webhook.sh --check    # fail if the chart copy is stale (make chart-webhook-check)
#
# The --check mode backs the CI drift gate (manifest-validate.yml) and `make check`.
set -euo pipefail

REPO_ROOT="$(git rev-parse --show-toplevel)"
cd "$REPO_ROOT"

# Temp file used by --check; cleaned up on exit. Initialised empty so the EXIT
# trap never trips `set -u` regardless of which path the script exits from.
TMP_WEBHOOK=""
trap 'rm -f "$TMP_WEBHOOK"' EXIT

# Authoritative controller-gen source (owned by `make -C cmd/gmc manifests`,
# generated from the +kubebuilder:webhook marker on the ActionsGateway webhook).
SRC_WEBHOOK="cmd/gmc/config/webhook/manifests.yaml"

# Generated chart template (derived — do not hand-edit; run `make chart-webhook`).
DST_WEBHOOK="charts/actions-gateway/templates/webhook.yaml"

# Helm template comment banner. Uses `{{- ... -}}` whitespace chomping so it warns
# a hand-editor in the source file but renders to nothing — `helm template` output
# is byte-identical to the pre-Q143 hand-maintained template.
BANNER="$(cat <<'EOF'
{{- /*
Generated from cmd/gmc/config/webhook/manifests.yaml by 'make chart-webhook' — DO NOT EDIT.
The webhook body (rules, failurePolicy, path, sideEffects, admissionReviewVersions) is the
controller-gen output of the +kubebuilder:webhook marker; this template re-injects the chart's
Helm wiring (name prefix, labels, namespace, cert-manager CA inject / caBundle). Re-run
'make chart-webhook' after any webhook-marker change. Drift is gated by 'make chart-webhook-check'.
*/ -}}
EOF
)"

# Replaces the controller-gen `metadata:` + name line. The chart prefixes the
# name, attaches the standard labels, and (when cert-manager is enabled) carries
# the ca-injector annotation that fills the caBundle.
META_BLOCK="$(cat <<'EOF'
metadata:
  name: {{ include "actions-gateway.namePrefix" . }}-validating-webhook-configuration
  labels:
    {{- include "actions-gateway.labels" . | nindent 4 }}
  {{- if .Values.certManager.enabled }}
  annotations:
    # cert-manager's ca-injector copies the Issuer's CA into the caBundle below.
    cert-manager.io/inject-ca-from: {{ printf "%s/serving-cert" .Release.Namespace }}
  {{- end }}
EOF
)"

# Injected at the clientConfig level, right after the service `path:` line. When
# cert-manager is disabled the chart generates a self-signed serving cert at
# render time and inlines its CA here; with cert-manager the ca-injector fills it.
CABUNDLE_BLOCK="$(cat <<'EOF'
    {{- if not .Values.certManager.enabled }}
    {{- include "actions-gateway.webhookCerts" . }}
    caBundle: {{ b64enc $.agWebhookCerts.caCert }}
    {{- end }}
EOF
)"

# render SRC DST — transform the controller-gen webhook manifest into the chart
# template. The webhook body is copied verbatim; only the metadata, the service
# namespace, and the caBundle block are chart-specific. Blocks are passed through
# the environment (ENVIRON), not `-v`: a multi-line `-v` assignment is rejected by
# BSD awk (macOS), while ENVIRON is portable across BSD awk and gawk.
render() {
	local src="$1" dst="$2"
	BANNER="$BANNER" META="$META_BLOCK" CABUNDLE="$CABUNDLE_BLOCK" awk '
		BEGIN { print ENVIRON["BANNER"] }
		# Drop the leading controller-gen document separator.
		NR == 1 && /^---$/ { next }
		# Replace the metadata block and drop the source name line that follows it.
		/^metadata:$/ {
			printf "%s\n", ENVIRON["META"]
			meta_injected = 1
			skip_name = 1
			next
		}
		skip_name && /^  name:/ { skip_name = 0; next }
		# Template the service namespace.
		/^      namespace: system$/ {
			print "      namespace: {{ .Release.Namespace }}"
			ns_injected = 1
			next
		}
		# Emit the verbatim path line, then the caBundle block beneath it.
		/^      path: / {
			print
			printf "%s\n", ENVIRON["CABUNDLE"]
			ca_injected = 1
			next
		}
		{ print }
		END {
			if (!meta_injected) {
				print "sync-chart-webhook: no top-level metadata: line in source" > "/dev/stderr"
				exit 1
			}
			if (!ns_injected) {
				print "sync-chart-webhook: no service namespace: system line in source" > "/dev/stderr"
				exit 1
			}
			if (!ca_injected) {
				print "sync-chart-webhook: no service path: line in source" > "/dev/stderr"
				exit 1
			}
		}
	' "$src" > "$dst"
}

sync() {
	render "$SRC_WEBHOOK" "$DST_WEBHOOK"
}

# check renders the chart webhook to a temp file and compares it against the
# on-disk chart copy — it does NOT mutate the working tree, so it detects drift in
# the committed chart (and any uncommitted hand-edit), not just whether a regen
# produced a git diff.
check() {
	TMP_WEBHOOK="$(mktemp)"
	render "$SRC_WEBHOOK" "$TMP_WEBHOOK"
	if ! diff -u "$DST_WEBHOOK" "$TMP_WEBHOOK"; then
		echo "ERROR: $DST_WEBHOOK is out of sync with $SRC_WEBHOOK." >&2
		echo "Re-sync and commit: make chart-webhook" >&2
		exit 1
	fi
	echo "chart webhook template is in sync with $SRC_WEBHOOK."
}

main() {
	case "${1:-}" in
	--check)
		check
		;;
	"")
		sync
		echo "wrote $DST_WEBHOOK from $SRC_WEBHOOK."
		;;
	*)
		echo "usage: $0 [--check]" >&2
		exit 2
		;;
	esac
}

main "$@"

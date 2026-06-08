{{/*
Name-prefix for all GMC resources. Defaults to "gmc", which reproduces the
kustomize bases' `namePrefix: gmc-` so the operational docs (which reference
`gmc-controller-manager`, `gmc-system`, the `agc-tenant-role`, and the
`namespace-psa-guard` policy by name) stay accurate against a Helm install.
Override only if you must run two GMCs in one cluster.
*/}}
{{- define "actions-gateway.namePrefix" -}}
{{- default "gmc" .Values.namePrefix -}}
{{- end -}}

{{/*
Fully-qualified controller-manager name, e.g. "gmc-controller-manager".
*/}}
{{- define "actions-gateway.managerName" -}}
{{- printf "%s-controller-manager" (include "actions-gateway.namePrefix" .) -}}
{{- end -}}

{{/*
ServiceAccount name the GMC pod runs as. Kept equal to managerName so the
`namespace-psa-guard` ValidatingAdmissionPolicy's
`system:serviceaccount:<ns>:<sa>` match condition resolves correctly.
*/}}
{{- define "actions-gateway.serviceAccountName" -}}
{{- include "actions-gateway.managerName" . -}}
{{- end -}}

{{/*
Selector labels — the immutable identity used by the Deployment selector,
Service selectors, NetworkPolicy podSelector, PDB selector, and ServiceMonitor
selector. Must stay byte-for-byte stable across upgrades, so it carries only
the two labels the kustomize bases select on.
*/}}
{{- define "actions-gateway.selectorLabels" -}}
control-plane: controller-manager
app.kubernetes.io/name: gmc
{{- end -}}

{{/*
Common metadata labels applied to every rendered resource. Superset of the
selector labels plus the standard Helm/Kubernetes recommended labels. Never
use this block in a `selector:` — only the selectorLabels are stable there.
*/}}
{{- define "actions-gateway.labels" -}}
{{ include "actions-gateway.selectorLabels" . }}
app.kubernetes.io/part-of: actions-gateway
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{/*
Resolve a container image reference from a {repository, tag, digest} dict.
Digest pinning is the secure default: when .digest is set the ref is
`repository@sha256:…` and the tag is ignored. Pass the sub-map as `.image`:
  {{ include "actions-gateway.image" .Values.gmc.image }}
*/}}
{{- define "actions-gateway.image" -}}
{{- if .digest -}}
{{- printf "%s@%s" .repository .digest -}}
{{- else -}}
{{- printf "%s:%s" .repository (.tag | default "latest") -}}
{{- end -}}
{{- end -}}

{{/*
Self-signed webhook serving cert used when certManager.enabled=false.

Generates a CA + serving cert for webhook-service ONCE per render and memoizes
it on the root context ($.agWebhookCerts) so the ValidatingWebhookConfiguration
caBundle and the webhook-server-cert Secret share the same key material — a
second genSignedCert call would produce a mismatched pair. An existing Secret in
the cluster is reused via `lookup` so an in-place `helm upgrade` does not rotate
the cert; `helm template` (no cluster) cannot look up and so regenerates, which
is fine for offline rendering. Call `include "actions-gateway.webhookCerts" .`
before reading $.agWebhookCerts.
*/}}
{{- define "actions-gateway.webhookCerts" -}}
{{- if not (hasKey $ "agWebhookCerts") -}}
{{- $svc := "webhook-service" -}}
{{- $ns := $.Release.Namespace -}}
{{- $cn := printf "%s.%s.svc" $svc $ns -}}
{{- $altNames := list $cn (printf "%s.%s.svc.cluster.local" $svc $ns) -}}
{{- $days := int $.Values.certManager.selfSignedCertDurationDays -}}
{{- $existing := lookup "v1" "Secret" $ns "webhook-server-cert" -}}
{{- if and $existing $existing.data (hasKey $existing.data "tls.crt") (hasKey $existing.data "ca.crt") -}}
{{- $_ := set $ "agWebhookCerts" (dict "caCert" (b64dec (index $existing.data "ca.crt")) "tlsCert" (b64dec (index $existing.data "tls.crt")) "tlsKey" (b64dec (index $existing.data "tls.key"))) -}}
{{- else -}}
{{- $ca := genCA (printf "%s-ca" $svc) $days -}}
{{- $cert := genSignedCert $cn nil $altNames $days $ca -}}
{{- $_ := set $ "agWebhookCerts" (dict "caCert" $ca.Cert "tlsCert" $cert.Cert "tlsKey" $cert.Key) -}}
{{- end -}}
{{- end -}}
{{- end -}}

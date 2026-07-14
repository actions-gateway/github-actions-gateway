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
Resolve a container image reference from a {repository, tag, digest} dict, with
digest pinning enforced at RENDER time (Q307). When .digest is set the ref is
`repository@sha256:…` and the tag is ignored. When it is empty, rendering FAILS
with a per-image message naming which value to set, rather than silently falling
back to a mutable :latest tag (secure by default).

Every image the chart resolves — gmc, agc, proxy, wrapper — goes through here.
The GMC binary does re-check the AGC_IMAGE/PROXY_IMAGE refs it injects at startup
(validateImageDigest in cmd/gmc/cmd/main.go), but an unpinned WRAPPER_IMAGE — or
the GMC's own image, which has no runtime guard at all — would otherwise only
surface later as a GMC crash-loop, a poor operator experience. Failing closed at
render catches all four up front. The one escape hatch is the explicit
allowFloatingImageTags=true dev/test opt-out — the same knob that relaxes the
GMC's runtime AGC/proxy/wrapper check. `make manifest-validate` asserts each
image fails closed. Pass a dict of the image sub-map, its values-path prefix
(for the error message), and the flag:
  {{ include "actions-gateway.image" (dict "image" .Values.agc.image "name" "agc" "allowFloating" .Values.allowFloatingImageTags) }}
*/}}
{{- define "actions-gateway.image" -}}
{{- $img := .image -}}
{{- if $img.digest -}}
{{- printf "%s@%s" $img.repository $img.digest -}}
{{- else if .allowFloating -}}
{{- printf "%s:%s" $img.repository ($img.tag | default "latest") -}}
{{- else -}}
{{- fail (printf "%s.image must be pinned by digest: set %s.image.digest=sha256:<64 hex digits> (see docs/operations/install.md, \"Pin images by digest\"). DEV/TEST ONLY: set allowFloatingImageTags=true to allow a floating tag." .name .name) -}}
{{- end -}}
{{- end -}}

{{/*
Whether the GMC metrics server uses a cert-manager-issued serving cert that the
ServiceMonitor verifies (vs controller-runtime's self-signed cert scraped with
insecureSkipVerify). True only when metrics are on, the metrics cert-manager
toggle is on, AND certManager.enabled is on — the metrics Certificate reuses the
webhook's selfsigned-issuer, which only exists in that case. Emits "true" or "".
*/}}
{{- define "actions-gateway.metricsCertManagerEnabled" -}}
{{- if and .Values.metrics.enabled .Values.metrics.tls.certManager.enabled .Values.certManager.enabled -}}true{{- end -}}
{{- end -}}

{{/*
DNS name (and TLS serverName) of the GMC metrics Service, e.g.
"gmc-controller-manager-metrics-service.gmc-system.svc". Used both as a SAN on
the cert-manager metrics Certificate and as the ServiceMonitor tlsConfig
serverName so the two always agree.
*/}}
{{- define "actions-gateway.metricsServiceDNSName" -}}
{{- printf "%s-metrics-service.%s.svc" (include "actions-gateway.managerName" .) .Release.Namespace -}}
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

{{/*
The name of everything here: the release's, with the chart's where it lacks it. It leaves room for
the longest suffix in a name of 63 characters.
*/}}
{{- define "oom-reporter.fullname" -}}
{{- if contains .Chart.Name .Release.Name -}}
{{- .Release.Name | trunc 40 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 40 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}

{{/*
The service accounts. Grants and policies both take their names from here.
*/}}
{{- define "oom-reporter.agentName" -}}{{ include "oom-reporter.fullname" . }}-agent{{- end -}}
{{- define "oom-reporter.controllerName" -}}{{ include "oom-reporter.fullname" . }}-controller{{- end -}}

{{/*
A service account as the API server names its user. Pass (list . "oom-reporter.agentName").
*/}}
{{- define "oom-reporter.username" -}}
{{- $root := index . 0 -}}
{{- printf "system:serviceaccount:%s:%s" $root.Release.Namespace (include (index . 1) $root) -}}
{{- end -}}

{{/*
Selector labels of one component. Pass (list . "agent").
*/}}
{{- define "oom-reporter.selectorLabels" -}}
{{- $root := index . 0 -}}
app.kubernetes.io/name: {{ $root.Chart.Name }}
app.kubernetes.io/instance: {{ $root.Release.Name }}
app.kubernetes.io/component: {{ index . 1 }}
{{- end -}}

{{- define "oom-reporter.labels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{- end -}}

{{- define "oom-reporter.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else if .Values.allowFloatingImageTags -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default "latest") -}}
{{- else -}}
{{- fail "image must be pinned by digest: set image.digest=sha256:<64 hex digits>. DEV/TEST ONLY: set allowFloatingImageTags=true to allow a floating tag." -}}
{{- end -}}
{{- end -}}

{{/*
What may be done wherever the controller writes.
*/}}
{{- define "oom-reporter.podRules" -}}
- apiGroups: [""]
  resources: ["pods"]
  verbs: ["get", "list", "watch"]
- apiGroups: [""]
  resources: ["pods/status"]
  verbs: ["patch"]
- apiGroups: [""]
  resources: ["events"]
  verbs: ["create"]
{{- end -}}

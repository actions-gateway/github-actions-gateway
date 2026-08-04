{{- /*
Unmodified copy of crd-ref-docs' built-in markdown typeMembers template. A
--templates-dir replaces the whole built-in set, so it has to be present.
*/ -}}
{{- define "type_members" -}}
{{- $field := . -}}
{{- if eq $field.Name "metadata" -}}
Refer to Kubernetes API documentation for fields of `metadata`.
{{- else -}}
{{ markdownRenderFieldDoc $field.Doc }}
{{- end -}}
{{- end -}}

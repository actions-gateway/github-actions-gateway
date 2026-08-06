{{- /*
Fork of crd-ref-docs' built-in markdown typeMembers template, plus the shared
field_doc helper the type template's member and enum rows also render through.

markdownRenderFieldDoc turns every newline into <br />, so a doc comment reached
the table cell still wrapped at the Go source's ~80 columns and every description
broke mid-sentence at a width the browser never chose. field_doc joins the soft
wraps back into flowing prose; blank lines and list-item starts keep their break,
carried past markdownRenderFieldDoc's escaping as a sentinel because it collapses
a real <br /><br /> back to a single one.
*/ -}}
{{- define "field_doc" -}}
{{- $doc := regexReplaceAll "\n[ \t]*\n\\s*" . "@@GAGBR@@" -}}
{{- $doc = regexReplaceAll "\n[ \t]*([-*+] )" $doc "@@GAGBR@@${1}" -}}
{{- $doc = regexReplaceAll "[ \t]*\n[ \t]*" $doc " " -}}
{{- replace "@@GAGBR@@" "<br />" (markdownRenderFieldDoc (trim $doc)) -}}
{{- end -}}

{{- define "type_members" -}}
{{- $field := . -}}
{{- if eq $field.Name "metadata" -}}
Refer to Kubernetes API documentation for fields of `metadata`.
{{- else -}}
{{ template "field_doc" $field.Doc }}
{{- end -}}
{{- end -}}

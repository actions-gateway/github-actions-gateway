{{- /*
Fork of crd-ref-docs' built-in markdown gvDetails template. The one change is
the package doc: Go doc comments spell a section heading `# Title` (gofmt keeps
it), which renders as an h1 in the middle of the page and puts a package-internal
aside in the site TOC beside the kinds. Render those leads bold instead.
*/ -}}
{{- define "gvDetails" -}}
{{- $gv := . -}}

## {{ $gv.GroupVersionString }}

{{ regexReplaceAll "(?m)^#+ (.+)$" $gv.Doc "**${1}**" }}

{{- if $gv.Kinds  }}
### Resource Types
{{- range $gv.SortedKinds }}
- {{ $gv.TypeForKind . | markdownRenderTypeLink }}
{{- end }}
{{ end }}

{{ range $gv.SortedTypes }}
{{ template "type" . }}
{{ end }}

{{- end -}}

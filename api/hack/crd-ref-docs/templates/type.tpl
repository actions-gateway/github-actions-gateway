{{- /*
Fork of crd-ref-docs' built-in markdown type template. Three changes: the type
doc gets the same `# Title` godoc-heading treatment as gv_details.tpl; both doc
cells render through the field_doc helper in type_members.tpl (unwraps the Go
source's hard line breaks); and the validation list joins with <br /> instead of
trailing one, which left every validation cell ending in a blank line.
*/ -}}
{{- define "type" -}}
{{- $type := . -}}
{{- if markdownShouldRenderType $type -}}

#### {{ $type.Name }}

{{ if $type.IsAlias }}_Underlying type:_ _{{ markdownRenderTypeLink $type.UnderlyingType  }}_{{ end }}

{{ regexReplaceAll "(?m)^#+ (.+)$" $type.Doc "**${1}**" }}

{{ if $type.Validation -}}
_Validation:_
{{- range $type.Validation }}
- {{ . }}
{{- end }}
{{- end }}

{{ if $type.References -}}
_Appears in:_
{{- range $type.SortedReferences }}
- {{ markdownRenderTypeLink . }}
{{- end }}
{{- end }}

{{ if $type.Members -}}
| Field | Description | Default | Validation |
| --- | --- | --- | --- |
{{ if $type.GVK -}}
| `apiVersion` _string_ | `{{ $type.GVK.Group }}/{{ $type.GVK.Version }}` | | |
| `kind` _string_ | `{{ $type.GVK.Kind }}` | | |
{{ end -}}

{{ range $type.Members -}}
| `{{ .Name  }}` _{{ markdownRenderType .Type }}_ | {{ template "type_members" . }} | {{ markdownRenderDefault .Default }} | {{ range $i, $v := .Validation }}{{ if $i }}<br />{{ end }}{{ markdownRenderFieldDoc $v }}{{ end }} |
{{ end -}}

{{ end -}}

{{ if $type.EnumValues -}}
| Field | Description |
| --- | --- |
{{ range $type.EnumValues -}}
| `{{ .Name }}` | {{ template "field_doc" .Doc }} |
{{ end -}}
{{ end -}}


{{- end -}}
{{- end -}}

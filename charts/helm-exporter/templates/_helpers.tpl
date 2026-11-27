{{- define "helm-exporter.fullname" -}}
{{- if contains .Chart.Name .Release.Name }}{{ .Release.Name | trunc 63 | trimSuffix "-" }}{{ else }}{{ printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" }}{{ end }}
{{- end }}

{{- define "helm-exporter.selectorLabels" -}}
app.kubernetes.io/name: {{ .Chart.Name }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "helm-exporter.labels" -}}
{{ include "helm-exporter.selectorLabels" . }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end }}

{{/* A list or a comma-separated string, as a list. */}}
{{- define "helm-exporter.list" -}}
{{- if kindIs "string" . }}{{ splitList "," . | compact | toJson }}{{ else }}{{ . | default list | toJson }}{{ end }}
{{- end }}

{{- define "helm-exporter.namespaces" -}}
{{- include "helm-exporter.list" .Values.namespaces }}
{{- end }}

{{- define "helm-exporter.image" -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) }}
{{- end }}

{{- define "comporta.name" -}}
{{- printf "%s" .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "comporta.labels" -}}
app.kubernetes.io/name: comporta
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end -}}

{{- define "comporta.selector" -}}
app.kubernetes.io/name: comporta
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{- define "comporta.image" -}}
{{- if .Values.image.digest -}}
{{ .Values.image.repository }}@{{ .Values.image.digest }}
{{- else -}}
{{ .Values.image.repository }}:{{ .Values.image.tag }}
{{- end -}}
{{- end -}}

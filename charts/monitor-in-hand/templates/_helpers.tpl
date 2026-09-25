{{- define "monitor-in-hand.name" -}}
monitor-in-hand
{{- end }}

{{- define "monitor-in-hand.fullname" -}}
{{- printf "%s-%s" .Release.Name (include "monitor-in-hand.name" .) | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "monitor-in-hand.labels" -}}
app.kubernetes.io/name: {{ include "monitor-in-hand.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" }}
{{- end }}

{{- define "monitor-in-hand.selectorLabels" -}}
app.kubernetes.io/name: {{ include "monitor-in-hand.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{- define "monitor-in-hand.validateValues" -}}
{{- if and (hasKey .Values.nodeSelector "kubernetes.io/os") (ne (index .Values.nodeSelector "kubernetes.io/os") "linux") -}}
{{- fail "nodeSelector kubernetes.io/os 与固定 linux selector 冲突" -}}
{{- end -}}
{{- end }}

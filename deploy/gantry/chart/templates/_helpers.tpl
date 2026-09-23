{{- define "gantry.image" -}}
{{- if and .Values.image.reference .Values.image.digest -}}
{{- fail "image.reference and image.digest are mutually exclusive" -}}
{{- end -}}
{{- if .Values.image.reference -}}
{{- .Values.image.reference -}}
{{- else if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end }}

{{- define "gantry.validate" -}}
{{- $mountPath := trimSuffix "/" .Values.containerd.mountPath -}}
{{- $socketPrefix := printf "%s/" $mountPath -}}
{{- if not (hasPrefix $socketPrefix .Values.containerd.socketPath) -}}
{{- fail "containerd.socketPath must be located under containerd.mountPath" -}}
{{- end -}}
{{- end }}

{{- define "gantry.labels" -}}
app.kubernetes.io/name: gantry
{{- if eq .Values.manager "helm" }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- else }}
app.kubernetes.io/managed-by: {{ .Values.manager | quote }}
{{- end }}
{{- end }}
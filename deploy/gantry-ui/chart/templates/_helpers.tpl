{{- define "gantry-ui.fullname" -}}
{{- /* Hash the full release and namespace so cluster and cross-namespace names remain unique. */ -}}
{{- printf "%s-gantry-ui-%s" (.Release.Name | trunc 23 | trimSuffix "-") (printf "%s/%s" .Release.Namespace .Release.Name | sha256sum | trunc 8) -}}
{{- end }}

{{- define "gantry-ui.labels" -}}
app.kubernetes.io/name: gantry-ui
app.kubernetes.io/instance: {{ .Release.Name | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | quote }}
{{- end }}

{{- define "gantry-ui.image" -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- end }}

{{- define "gantry-ui.validate" -}}
{{- if and .Values.builder.enabled (not (hasPrefix (printf "%s/" .Values.registry) .Values.builder.base)) -}}
{{- fail "builder.base must be a digest-pinned image in the configured registry" -}}
{{- end -}}
{{- end }}

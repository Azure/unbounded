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

{{- define "gantry.overlaybdConfigImage" -}}
{{- if and .Values.overlaybdConfig.image.reference .Values.overlaybdConfig.image.digest -}}
{{- fail "overlaybdConfig.image.reference and overlaybdConfig.image.digest are mutually exclusive" -}}
{{- end -}}
{{- if .Values.overlaybdConfig.image.reference -}}
{{- .Values.overlaybdConfig.image.reference -}}
{{- else if .Values.overlaybdConfig.image.digest -}}
{{- printf "%s@%s" .Values.overlaybdConfig.image.repository .Values.overlaybdConfig.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.overlaybdConfig.image.repository (default .Chart.AppVersion .Values.overlaybdConfig.image.tag) -}}
{{- end -}}
{{- end }}

{{- define "gantry.validate" -}}
{{- $mountPath := trimSuffix "/" .Values.containerd.mountPath -}}
{{- $socketPrefix := printf "%s/" $mountPath -}}
{{- if not (hasPrefix $socketPrefix .Values.containerd.socketPath) -}}
{{- fail "containerd.socketPath must be located under containerd.mountPath" -}}
{{- end -}}
{{- if and .Values.overlaybdConfig.enabled (not .Values.gantry.artifactStreaming.enabled) -}}
{{- fail "overlaybdConfig.enabled requires gantry.artifactStreaming.enabled" -}}
{{- end -}}
{{- if and .Values.overlaybdConfig.enabled (empty .Values.overlaybdConfig.nodeSelector) -}}
{{- fail "overlaybdConfig.enabled requires an explicit overlaybdConfig.nodeSelector" -}}
{{- end -}}
{{- end }}

{{- define "gantry.managerLabel" -}}
{{- if eq .Values.manager "helm" }}
app.kubernetes.io/managed-by: {{ .Release.Service | quote }}
{{- else }}
app.kubernetes.io/managed-by: {{ .Values.manager | quote }}
{{- end }}
{{- end }}

{{- define "gantry.labels" -}}
app.kubernetes.io/name: gantry
{{ include "gantry.managerLabel" . }}
{{- end }}

{{- define "gantry.nodeConfigDefaultHosts" -}}
# Managed by the Gantry Helm chart.
[host."http://127.0.0.1:5000"]
	capabilities = ["pull", "resolve"]
	dial_timeout = "200ms"
{{- end }}

{{- define "gantry.nodeConfigRegistryHosts" -}}
# Managed by the Gantry Helm chart.
server = "https://{{ .name }}"

[host."http://127.0.0.1:5000"]
	capabilities = ["pull", "resolve"]
	dial_timeout = "200ms"

# Preserve AKS Artifact Streaming when Gantry is unavailable.
[host."http://127.0.0.1:8578"]
	capabilities = ["pull", "resolve"]
{{- end }}

{{- define "gantry.nodeConfigPayload" -}}
{{- if .Values.gantry.artifactStreaming.enabled -}}
{{- range $index, $registry := .Values.gantry.upstreamRegistries }}
hosts-{{ $index }}.toml: |
  {{- include "gantry.nodeConfigRegistryHosts" $registry | nindent 2 }}
{{- end }}
{{- else }}
hosts.toml: |
  {{- include "gantry.nodeConfigDefaultHosts" . | nindent 2 }}
{{- end }}
{{- end }}
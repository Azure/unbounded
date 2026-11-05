{{- define "unbounded-operator.labels" -}}
app.kubernetes.io/name: unbounded-operator
app.kubernetes.io/component: controller
{{- end }}

{{- define "unbounded-operator.registry" -}}
{{- trimSuffix "/" .Values.imageRegistry -}}
{{- end }}

{{- define "unbounded-operator.image" -}}
{{- if .Values.image.reference -}}
{{- .Values.image.reference -}}
{{- else -}}
{{- $repo := printf "%s/%s" (include "unbounded-operator.registry" .) .Values.image.name -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" $repo .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" $repo (default .Chart.AppVersion .Values.image.tag) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
sha256 of the canonical (sorted, compact) JSON of the ConfigMap data. Must match
operatorConfigHash in cmd/kubectl-unbounded/app/install.go so a re-render or
`kubectl unbounded install` with changed settings rolls the operator.
*/}}
{{- define "unbounded-operator.configHash" -}}
{{- dict "UNBOUNDED_API_SERVER_ENDPOINT" .Values.apiServerEndpoint "UNBOUNDED_IMAGE_REGISTRY" (include "unbounded-operator.registry" .) "UNBOUNDED_REAP_LEGACY_RESOURCES" (toString .Values.reapLegacyResources) | toJson | sha256sum -}}
{{- end }}

{{/*
Refuse to take over an operator Deployment this release doesn't own, such as
one created by `kubectl unbounded install`. lookup returns nothing during
`helm template` (which renders deploy/unbounded-operator/rendered), so this
only runs against a live cluster.
*/}}
{{- define "unbounded-operator.ownershipCheck" -}}
{{- $existing := lookup "apps/v1" "Deployment" .Release.Namespace "unbounded-operator" -}}
{{- if $existing -}}
{{- $annotations := default dict $existing.metadata.annotations -}}
{{- $releaseName := default "" (get $annotations "meta.helm.sh/release-name") -}}
{{- $releaseNamespace := default "" (get $annotations "meta.helm.sh/release-namespace") -}}
{{- if not (and (eq $releaseName .Release.Name) (eq $releaseNamespace .Release.Namespace)) -}}
{{- fail (printf "Deployment %s/unbounded-operator already exists and isn't managed by this Helm release; it was likely installed by `kubectl unbounded install`. Remove that installation before installing the chart." .Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end }}

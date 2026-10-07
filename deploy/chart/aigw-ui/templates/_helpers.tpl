{{- define "aigw-ui.name" -}}
{{- .Release.Name | trunc 50 | trimSuffix "-" -}}
{{- end -}}

{{- define "aigw-ui.labels" -}}
app.kubernetes.io/name: aigw-ui
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "aigw-ui.selector" -}}
app.kubernetes.io/name: aigw-ui
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* image <global registry> <image values> <default tag> */}}
{{- define "aigw-ui.image" -}}
{{- $registry := index . 0 -}}
{{- $img := index . 1 -}}
{{- $tag := $img.tag | default (index . 2) -}}
{{- if $registry -}}
{{- /* Drop the image's own registry host so the mirror path stays predictable. */ -}}
{{- $parts := splitList "/" $img.repository -}}
{{- $first := first $parts -}}
{{- $repo := $img.repository -}}
{{- if and (gt (len $parts) 1) (or (contains "." $first) (contains ":" $first)) -}}
{{- $repo = join "/" (rest $parts) -}}
{{- end -}}
{{- printf "%s/%s:%s" (trimSuffix "/" $registry) $repo $tag -}}
{{- else -}}
{{- printf "%s:%s" $img.repository $tag -}}
{{- end -}}
{{- end -}}

{{- define "aigw-ui.authSecret" -}}
{{- .Values.auth.existingSecret | default (printf "%s-auth" (include "aigw-ui.name" .)) -}}
{{- end -}}

{{- define "aigw-ui.pullSecrets" -}}
{{- with .Values.global.imagePullSecrets }}
imagePullSecrets:
  {{- range . }}
  - name: {{ . }}
  {{- end }}
{{- end }}
{{- end -}}

{{/* Settings every container gets. No runAsUser: OpenShift assigns one. */}}
{{- define "aigw-ui.containerSecurity" -}}
allowPrivilegeEscalation: false
capabilities:
  drop: ["ALL"]
runAsNonRoot: true
seccompProfile:
  type: RuntimeDefault
{{- end -}}

{{/*
Expand the name of the chart.
*/}}
{{- define "synology-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
We truncate at 63 chars because some Kubernetes name fields are limited to this (by the DNS naming spec).
If release name contains chart name it will be used as a full name.
*/}}
{{- define "synology-csi.fullname" -}}
{{- if .Values.fullnameOverride }}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- $name := default .Chart.Name .Values.nameOverride }}
{{- if contains $name .Release.Name }}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- else }}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Create chart name and version as used by the chart label.
*/}}
{{- define "synology-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "synology-csi.labels" -}}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
{{ include "synology-csi.selectorLabels" . }}
helm.sh/chart: {{ include "synology-csi.chart" . }}
{{- end }}

{{/*
Selector Labels:
*/}}
{{- define "synology-csi.selectorLabels" -}}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/name: {{ include "synology-csi.name" . }}
helm.sh/template: {{ .Template.Name | trimPrefix .Template.BasePath | trimPrefix "/" | replace "/" "_" }}
{{- end }}

{{/*
Client Info Secret Volume:
*/}}
{{- define "synology-csi.clientInfoSecretVolume" -}}
name: client-info
secret:
  secretName: {{ .Values.clientInfoSecret.name | default (include "synology-csi.fullname" . | printf "%s-client-info") }}
{{- end }}

{{/*
node.iscsi.replacementTimeout as a whole number of seconds. Fails the render for
anything but an integer from 0 to 2147483647 (the kernel's int recovery_tmo),
instead of letting Helm's int turn it into 0 or saturate it. Every check runs on
the value as given, before it is converted.
*/}}
{{- define "synology-csi.iscsiReplacementTimeout" -}}
{{- $max := 2147483647 -}}
{{- $v := . -}}
{{- $err := printf "node.iscsi.replacementTimeout must be an integer number of seconds from 0 to %d, got %v" $max (toJson $v) -}}
{{- if kindIs "invalid" $v -}}
0
{{- else if kindIs "string" $v -}}
{{- if not (regexMatch "^[0-9]+$" $v) -}}{{- fail $err -}}{{- end -}}
{{- /* Leading zeros dropped, so the length check counts significant digits; 10 digits always fit in atoi. */ -}}
{{- $digits := regexReplaceAll "^0+" $v "" -}}
{{- if gt (len $digits) 10 -}}{{- fail $err -}}{{- end -}}
{{- if and (gt (len $digits) 0) (gt (atoi $digits) $max) -}}{{- fail $err -}}{{- end -}}
{{- if eq (len $digits) 0 -}}0{{- else -}}{{- atoi $digits -}}{{- end -}}
{{- else if or (kindIs "float64" $v) (kindIs "int64" $v) (kindIs "int" $v) -}}
{{- if or (lt (float64 $v) 0.0) (gt (float64 $v) (float64 $max)) (ne (float64 $v) (floor (float64 $v))) -}}{{- fail $err -}}{{- end -}}
{{- int64 $v -}}
{{- else -}}
{{- fail $err -}}
{{- end -}}
{{- end }}

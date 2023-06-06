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

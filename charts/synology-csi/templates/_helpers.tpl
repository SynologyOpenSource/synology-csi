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
Create controller name
*/}}
{{- define "synology-csi.controller.fullname" -}}
{{- printf "%s-controller" (include "synology-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create node name
*/}}
{{- define "synology-csi.node.fullname" -}}
{{- printf "%s-node" (include "synology-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{/*
Create snapshotter name
*/}}
{{- define "synology-csi.snapshotter.fullname" -}}
{{- printf "%s-snapshotter" (include "synology-csi.fullname" .) | trunc 63 | trimSuffix "-" -}}
{{- end -}}

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
helm.sh/chart: {{ include "synology-csi.chart" . }}
{{ include "synology-csi.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "synology-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "synology-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use for the controller
*/}}
{{- define "synology-csi.controller.serviceAccountName" -}}
{{- if .Values.controller.serviceAccount.create }}
{{- default (include "synology-csi.controller.fullname" .) .Values.controller.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.controller.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use for the node
*/}}
{{- define "synology-csi.node.serviceAccountName" -}}
{{- if .Values.node.serviceAccount.create }}
{{- default (include "synology-csi.node.fullname" .) .Values.node.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.node.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
Create the name of the service account to use for the snapshotter
*/}}
{{- define "synology-csi.snapshotter.serviceAccountName" -}}
{{- if .Values.snapshotter.serviceAccount.create }}
{{- default (include "synology-csi.snapshotter.fullname" .) .Values.snapshotter.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.snapshotter.serviceAccount.name }}
{{- end }}
{{- end }}

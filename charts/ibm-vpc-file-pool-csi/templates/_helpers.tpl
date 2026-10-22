{{/*
Expand the name of the chart.
*/}}
{{- define "ibm-vpc-file-pool-csi.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "ibm-vpc-file-pool-csi.fullname" -}}
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
Chart label value.
*/}}
{{- define "ibm-vpc-file-pool-csi.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels.
*/}}
{{- define "ibm-vpc-file-pool-csi.labels" -}}
helm.sh/chart: {{ include "ibm-vpc-file-pool-csi.chart" . }}
app.kubernetes.io/name: {{ include "ibm-vpc-file-pool-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels.
*/}}
{{- define "ibm-vpc-file-pool-csi.selectorLabels" -}}
app.kubernetes.io/name: {{ include "ibm-vpc-file-pool-csi.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
"true" when the cluster has the OpenShift console, so the console plugin and
its hook Jobs can be installed there.
*/}}
{{- define "ibm-vpc-file-pool-csi.hasConsole" -}}
{{- if .Capabilities.APIVersions.Has "console.openshift.io/v1/ConsolePlugin" }}true{{- end }}
{{- end }}

{{/*
"true" when the console plugin is enabled and the cluster has the OpenShift console.
*/}}
{{- define "ibm-vpc-file-pool-csi.consolePlugin" -}}
{{- if and .Values.consolePlugin.enabled (include "ibm-vpc-file-pool-csi.hasConsole" .) }}true{{- end }}
{{- end }}

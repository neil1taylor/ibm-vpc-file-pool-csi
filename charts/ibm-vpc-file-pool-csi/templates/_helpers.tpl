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

{{/*
"true" when the alerts and scraping go in the monitoring namespace that
OpenShift's user workload monitoring reads. Platform monitoring reads only
namespaces labelled openshift.io/cluster-monitoring=true, and Red Hat says not
to label other namespaces, so the driver (in kube-system) cannot be read there.
mode auto: on when the cluster has the Prometheus Operator API and the OpenShift
console and the certificate provider is "openshift" (the CA bundle comes from
the service CA). mode enabled: always on, and fails with another provider.
*/}}
{{- define "ibm-vpc-file-pool-csi.uwm" -}}
{{- $mode := .Values.metrics.monitoring.mode -}}
{{- if not (has $mode (list "auto" "enabled" "disabled")) }}{{- fail "metrics.monitoring.mode must be auto, enabled or disabled" }}{{- end -}}
{{- if eq $mode "enabled" -}}
{{- if ne .Values.webhook.certProvider "openshift" }}{{- fail "metrics.monitoring.mode=enabled needs webhook.certProvider=openshift: the scraper's CA bundle comes from the OpenShift service CA" }}{{- end -}}
true
{{- else if and (eq $mode "auto") (eq .Values.webhook.certProvider "openshift") (.Capabilities.APIVersions.Has "monitoring.coreos.com/v1/ServiceMonitor") (.Capabilities.APIVersions.Has "console.openshift.io/v1/ConsolePlugin") -}}
true
{{- end -}}
{{- end }}

{{/*
Endpoint settings of a ServiceMonitor in the release namespace that scrapes a
metrics Service over HTTPS. Call with (dict "root" . "svc" <Service name>).
The certificate is checked against the Service name; its CA is the OpenShift
service CA bundle, or ca.crt of the Service's certificate secret.
*/}}
{{- define "ibm-vpc-file-pool-csi.scrapeTLS" -}}
interval: {{ .root.Values.metrics.serviceMonitor.interval }}
scheme: https
bearerTokenFile: /var/run/secrets/kubernetes.io/serviceaccount/token
tlsConfig:
  serverName: {{ .svc }}.{{ .root.Release.Namespace }}.svc
  {{- if eq .root.Values.webhook.certProvider "openshift" }}
  caFile: /etc/prometheus/configmaps/serving-certs-ca-bundle/service-ca.crt
  {{- else }}
  ca:
    secret:
      name: {{ .svc }}-certs
      key: ca.crt
  {{- end }}
{{- end }}

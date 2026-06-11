{{/*
Expand the name of the chart.
*/}}
{{- define "bolometer.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Create a default fully qualified app name.
*/}}
{{- define "bolometer.fullname" -}}
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
{{- define "bolometer.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Common labels
*/}}
{{- define "bolometer.labels" -}}
helm.sh/chart: {{ include "bolometer.chart" . }}
{{ include "bolometer.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{/*
Selector labels
*/}}
{{- define "bolometer.selectorLabels" -}}
app.kubernetes.io/name: {{ include "bolometer.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end }}

{{/*
Create the name of the service account to use
*/}}
{{- define "bolometer.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "bolometer.fullname" . ) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{/*
ServiceAccount annotations — merges user-provided annotations with the IRSA
annotation when aws.irsa.enabled=true so callers don't have to set both.
*/}}
{{- define "bolometer.serviceAccountAnnotations" -}}
{{- $annotations := dict -}}
{{- if .Values.serviceAccount.annotations -}}
{{-   $annotations = merge $annotations .Values.serviceAccount.annotations -}}
{{- end -}}
{{- if and .Values.aws.irsa.enabled .Values.aws.irsa.roleArn -}}
{{-   $_ := set $annotations "eks.amazonaws.com/role-arn" .Values.aws.irsa.roleArn -}}
{{- end -}}
{{- if $annotations -}}
{{- toYaml $annotations -}}
{{- end -}}
{{- end }}

{{/*
Name of the Secret that holds static AWS credentials.
Resolves to existingSecret if set, otherwise to <fullname>-aws-credentials.
*/}}
{{- define "bolometer.awsSecretName" -}}
{{- if .Values.aws.staticCredentials.existingSecret -}}
{{- .Values.aws.staticCredentials.existingSecret -}}
{{- else -}}
{{- printf "%s-aws-credentials" (include "bolometer.fullname" .) -}}
{{- end -}}
{{- end }}

{{/* Helpers for the coredevs chart. */}}
{{- define "coredevs.name" -}}
{{- default .Chart.Name .Values.nameOverride | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "coredevs.fullname" -}}
{{- if .Values.fullnameOverride -}}
{{- .Values.fullnameOverride | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- $name := default .Chart.Name .Values.nameOverride -}}
{{- if contains $name .Release.Name -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" -}}
{{- else -}}
{{- printf "%s-%s" .Release.Name $name | trunc 63 | trimSuffix "-" -}}
{{- end -}}
{{- end -}}
{{- end -}}

{{- define "coredevs.labels" -}}
app.kubernetes.io/name: {{ include "coredevs.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end -}}

{{- define "coredevs.selectorLabels" -}}
app.kubernetes.io/name: {{ include "coredevs.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
{{- end -}}

{{/* DB connection env shared by the writer and reader pods. The DSN is composed
from the password secret so only the password lives in SOPS. */}}
{{- define "coredevs.dbEnv" -}}
- name: COREDEVS_DB_PASSWORD
  valueFrom:
    secretKeyRef:
      name: {{ include "coredevs.fullname" . }}-db
      key: password
- name: COREDEVS_DATABASE_URL
  value: "postgres://{{ .Values.postgres.user }}:$(COREDEVS_DB_PASSWORD)@{{ include "coredevs.fullname" . }}-postgres:5432/{{ .Values.postgres.database }}?sslmode=disable"
{{- end -}}

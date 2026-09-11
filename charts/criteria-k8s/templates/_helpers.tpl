{{/* Target namespace: values.namespace wins; empty falls back to the release namespace. */}}
{{- define "criteria-k8s.namespace" -}}
{{- default .Release.Namespace .Values.namespace -}}
{{- end -}}

{{/* Chart-level labels applied to every managed resource. */}}
{{- define "criteria-k8s.labels" -}}
app.kubernetes.io/part-of: criteria-k8s
helm.sh/chart: criteria-k8s-{{ .Chart.Version | replace "+" "_" }}
{{- end -}}

{{/* Pod security context shared by operator, watcher and castle pods. */}}
{{- define "criteria-k8s.podSecurityContext" -}}
securityContext:
{{ toYaml .Values.podSecurityContext | indent 2 }}
{{- end -}}

{{/* Node scheduling shared by operator and watcher pods. */}}
{{- define "criteria-k8s.scheduling" -}}
nodeSelector:
{{ toYaml .Values.nodeSelector | indent 2 }}
tolerations:
{{ toYaml .Values.tolerations | indent 2 }}
{{- end -}}

{{/* Restricted container security context (drop capabilities, no escalation). */}}
{{- define "criteria-k8s.restrictedContainer" -}}
securityContext:
  allowPrivilegeEscalation: false
  readOnlyRootFilesystem: true
  capabilities:
    drop:
      - ALL
{{- end -}}
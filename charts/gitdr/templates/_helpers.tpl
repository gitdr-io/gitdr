{{- define "gitdr.name" -}}{{ .Chart.Name }}{{- end -}}

{{- define "gitdr.fullname" -}}
{{- printf "%s-%s" .Release.Name .Chart.Name | trunc 63 | trimSuffix "-" -}}
{{- end -}}

{{- define "gitdr.labels" -}}
app.kubernetes.io/name: {{ include "gitdr.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ printf "%s-%s" .Chart.Name .Chart.Version }}
{{- end -}}

{{- define "gitdr.serviceAccountName" -}}
{{- if .Values.serviceAccount.create -}}
{{- default (include "gitdr.fullname" .) .Values.serviceAccount.name -}}
{{- else -}}
{{- default "default" .Values.serviceAccount.name -}}
{{- end -}}
{{- end -}}

{{- define "gitdr.image" -}}
{{- if .Values.image.digest -}}
{{- printf "%s@%s" .Values.image.repository .Values.image.digest -}}
{{- else -}}
{{- printf "%s:%s" .Values.image.repository (.Values.image.tag | default .Chart.AppVersion) -}}
{{- end -}}
{{- end -}}

{{/* Pod template shared by the CronJob and the one-shot Job. */}}
{{- define "gitdr.podTemplate" -}}
metadata:
  labels:
    {{- include "gitdr.labels" . | nindent 4 }}
spec:
  restartPolicy: {{ .Values.restartPolicy }}
  serviceAccountName: {{ include "gitdr.serviceAccountName" . }}
  securityContext:
    {{- toYaml .Values.podSecurityContext | nindent 4 }}
  containers:
    - name: gitdr
      image: {{ include "gitdr.image" . }}
      imagePullPolicy: {{ .Values.image.pullPolicy }}
      args:
        {{- toYaml .Values.args | nindent 8 }}
      securityContext:
        {{- toYaml .Values.securityContext | nindent 8 }}
      env:
        - name: HOME
          value: /tmp
        {{- with .Values.env }}
        {{- toYaml . | nindent 8 }}
        {{- end }}
      {{- with .Values.envFrom }}
      envFrom:
        {{- toYaml . | nindent 8 }}
      {{- end }}
      volumeMounts:
        - name: config
          mountPath: /etc/gitdr
          readOnly: true
        - name: tmp
          mountPath: /tmp
      {{- with .Values.resources }}
      resources:
        {{- toYaml . | nindent 8 }}
      {{- end }}
  volumes:
    - name: config
      configMap:
        name: {{ include "gitdr.fullname" . }}-config
    - name: tmp
      emptyDir: {}
  {{- with .Values.nodeSelector }}
  nodeSelector:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.affinity }}
  affinity:
    {{- toYaml . | nindent 4 }}
  {{- end }}
  {{- with .Values.tolerations }}
  tolerations:
    {{- toYaml . | nindent 4 }}
  {{- end }}
{{- end -}}

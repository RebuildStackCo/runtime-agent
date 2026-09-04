{{/*
Names.

The two component names are fixed rather than derived from the release name,
and that is a decision rather than an omission (ADR 0036). The node
authenticates to the controller with a projected token whose subject the
controller pins by name, and docs/security.md prints that exact string. A pair
of stable names keeps the audit story readable and keeps one agent per cluster
from being installed twice by accident.

Cluster-scoped objects carry the namespace, because two releases in two
namespaces would otherwise collide on one ClusterRole.
*/}}
{{- define "runtime-agent.controllerName" -}}
runtime-agent-controller
{{- end -}}

{{- define "runtime-agent.nodeName" -}}
runtime-agent-node
{{- end -}}

{{- define "runtime-agent.clusterRoleName" -}}
runtime-agent-controller-{{ .Release.Namespace }}
{{- end -}}

{{/* The subject the controller accepts on node reports: this release's node SA. */}}
{{- define "runtime-agent.nodeSubject" -}}
system:serviceaccount:{{ .Release.Namespace }}:{{ include "runtime-agent.nodeName" . }}
{{- end -}}

{{/* The in-cluster base URL of the controller's receiver. */}}
{{- define "runtime-agent.controllerEndpoint" -}}
http://{{ include "runtime-agent.controllerName" . }}.{{ .Release.Namespace }}.svc:8080
{{- end -}}

{{- define "runtime-agent.image" -}}
{{ .Values.image.repository }}:{{ .Values.image.tag | default .Chart.AppVersion }}
{{- end -}}

{{- define "runtime-agent.commonLabels" -}}
app.kubernetes.io/name: runtime-agent
app.kubernetes.io/instance: {{ .Release.Name }}
app.kubernetes.io/version: {{ .Values.image.tag | default .Chart.AppVersion | quote }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
helm.sh/chart: {{ .Chart.Name }}-{{ .Chart.Version }}
{{- end -}}

{{/*
Whether the node DaemonSet is installed at all, and whether it profiles.
Both answers come from the profile and from nothing else, so a component cannot
be half-enabled.
*/}}
{{- define "runtime-agent.nodeEnabled" -}}
{{- if or (eq .Values.profile "inventory") (eq .Values.profile "ebpf") -}}true{{- end -}}
{{- end -}}

{{- define "runtime-agent.ebpfEnabled" -}}
{{- if eq .Values.profile "ebpf" -}}true{{- end -}}
{{- end -}}

{{/*
Validation the JSON schema cannot express, called from every template that
renders a workload so it fires whatever is being rendered.
*/}}
{{- define "runtime-agent.validate" -}}
{{- if not (has .Values.profile (list "metrics-only" "inventory" "ebpf")) -}}
{{- fail (printf "unknown profile %q: expected metrics-only, inventory or ebpf" .Values.profile) -}}
{{- end -}}
{{- if ne .Release.Namespace "kube-system" -}}
{{- range $role := list "controller" "node" -}}
{{- $name := (index $.Values $role).priorityClassName -}}
{{- if hasPrefix "system-" $name -}}
{{- fail (printf "%s.priorityClassName %q: admission accepts a system- class only for pods in kube-system, and this release is in %q — the pods would be rejected, not scheduled at a lower priority (ADR 0068)" $role $name $.Release.Namespace) -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{/*
The controller's floor, and the only one: below the receiver's own 10s shutdown
budget the SIGKILL arrives before the flush pass, and the journals it would have
written are lost with nothing to notice it by (ADR 0068 §4). The node has no
floor beyond a positive value — it has no pass and no loss.
*/}}
{{- if lt (int .Values.controller.terminationGracePeriodSeconds) 15 -}}
{{- fail (printf "controller.terminationGracePeriodSeconds is %d: below 15 a SIGKILL lands before the shutdown flush, silently dropping up to a minute of the restart, disruption, job-run, node-lifecycle and inventory journals (ADR 0068)" (int .Values.controller.terminationGracePeriodSeconds)) -}}
{{- end -}}
{{- if lt (int .Values.node.terminationGracePeriodSeconds) 1 -}}
{{- fail "node.terminationGracePeriodSeconds must be at least 1: zero deletes the pod without giving the scan pass a chance to end" -}}
{{- end -}}
{{- end -}}

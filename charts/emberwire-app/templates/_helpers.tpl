{{/*
Name.

SYNC: must equal the Service name (.Release.Name). The Industrial Dashboard
reads app.kubernetes.io/name for Rancher proxy routing, and if this returns the
chart name ("emberwire-app") while the Service is named "emberwire-embernet003",
the proxy 404s.
*/}}
{{- define "emberwire-app.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name. Forced to .Release.Name so the dashboard can resolve
<release-name>.<namespace>.svc.cluster.local, and so two instances on one node
are distinguishable.
*/}}
{{- define "emberwire-app.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "emberwire-app.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "emberwire-app.labels" -}}
helm.sh/chart: {{ include "emberwire-app.chart" . }}
{{ include "emberwire-app.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- end }}

{{- define "emberwire-app.selectorLabels" -}}
app.kubernetes.io/name: {{ include "emberwire-app.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: {{ include "emberwire-app.fullname" . }}
{{- end }}

{{/*
EmberNET Store discovery labels — THE BIG FIVE.

On the pod template AND the Service. All five, always. Miss one and the app is
invisible to the dashboard; miss them on the Service specifically and it shows
in node detail but not in Running Apps.
*/}}
{{- define "emberwire-app.storeLabels" -}}
embernet.ai/store-app: "true"
embernet.ai/gui-type: {{ .Values.gui.type | default "web" | quote }}
embernet.ai/app-name: {{ include "emberwire-app.name" . | quote }}
embernet.ai/gui-port: {{ .Values.gui.port | default .Values.service.port | quote }}
embernet.ai/chart-name: {{ .Chart.Name | quote }}
{{- end }}

{{/*
Display name.

The dashboard injects .Values.embernet.displayName at deploy time and it must
win. Reading .Values.gui.displayName alone silently drops the injected value and
every node card shows the raw release name — the bug fixed in nodered-pod 2.2.4.
*/}}
{{- define "emberwire-app.storeAnnotations" -}}
{{- $dn := "" -}}
{{- with .Values.embernet }}{{- if .displayName }}{{- $dn = .displayName -}}{{- end }}{{- end }}
{{- if not $dn }}{{- $dn = .Values.gui.displayName -}}{{- end }}
{{- if $dn }}
embernet.ai/display-name: {{ $dn | quote }}
{{- end }}
{{- end }}

{{/*
Tenant labels, injected by the dashboard. Must be on BOTH the pod template and
the Service, or the app is visible only to SuperAdmin — the most common silent
App Store failure. A no-op on a bare helm install.
*/}}
{{- define "emberwire-app.tenantLabels" -}}
{{- with .Values.tenantLabels }}
{{- toYaml . }}
{{- end }}
{{- end }}

{{- define "emberwire-app.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "emberwire-app.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "emberwire-app.pvcName" -}}
{{- printf "%s-data" (include "emberwire-app.fullname" .) }}
{{- end }}

{{- define "emberwire-app.secretName" -}}
{{- printf "%s-auth" (include "emberwire-app.fullname" .) }}
{{- end }}

{{- define "emberwire-app.configMapName" -}}
{{- printf "%s-config" (include "emberwire-app.fullname" .) }}
{{- end }}

{{- define "emberwire-app.nadName" -}}
{{- printf "%s-macvlan" (include "emberwire-app.fullname" .) }}
{{- end }}

{{- define "emberwire-app.resources" -}}
{{- $preset := .Values.resources.preset | default "small" }}
{{- if eq $preset "custom" }}
{{- toYaml .Values.resources.custom }}
{{- else }}
{{- $presets := .Values.resources.presets }}
{{- if hasKey $presets $preset }}
{{- toYaml (index $presets $preset) }}
{{- else }}
{{- toYaml $presets.small }}
{{- end }}
{{- end }}
{{- end }}

{{/*
Credential secret.

Generated on first install and PRESERVED across upgrades by reading the existing
Secret back. This is not optional care: if the secret regenerates on an upgrade,
every credential already written to the PVC becomes undecryptable and every
broker password in every flow is lost. A bare randAlphaNum here would do exactly
that on the first `helm upgrade`.
*/}}
{{- define "emberwire-app.credentialSecret" -}}
{{- if .Values.emberwire.credentialSecret -}}
{{- .Values.emberwire.credentialSecret -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "emberwire-app.secretName" .) -}}
{{- if and $existing $existing.data (hasKey $existing.data "credential-secret") -}}
{{- index $existing.data "credential-secret" | b64dec -}}
{{- else -}}
{{- randAlphaNum 48 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Admin password. Same preservation rule: regenerating it on upgrade would lock
the operator out of their own editor.
*/}}
{{- define "emberwire-app.adminPassword" -}}
{{- if .Values.auth.password -}}
{{- .Values.auth.password -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "emberwire-app.secretName" .) -}}
{{- if and $existing $existing.data (hasKey $existing.data "admin-password") -}}
{{- index $existing.data "admin-password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 20 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Admin password hash.

An explicit hash wins. Otherwise the hash is preserved alongside the password so
that an upgrade does not invalidate a working login. bcrypt is generated by Helm
only when there is nothing to preserve.
*/}}
{{- define "emberwire-app.adminPasswordHash" -}}
{{- if .Values.auth.passwordHash -}}
{{- .Values.auth.passwordHash -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "emberwire-app.secretName" .) -}}
{{- if and $existing $existing.data (hasKey $existing.data "admin-password-hash") -}}
{{- index $existing.data "admin-password-hash" | b64dec -}}
{{- else -}}
{{- htpasswd .Values.auth.username (include "emberwire-app.adminPassword" .) | trimPrefix (printf "%s:" .Values.auth.username) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Validate the network mode and fail the render rather than producing a manifest
that is wrong in a way nobody notices until a pod is Pending.
*/}}
{{- define "emberwire-app.validateNetwork" -}}
{{- $mode := .Values.network.mode | default "cluster" -}}
{{- if not (has $mode (list "cluster" "host" "macvlan")) -}}
{{- fail (printf "network.mode must be cluster, host or macvlan; got %q" $mode) -}}
{{- end -}}
{{- if eq $mode "macvlan" -}}
{{- if .Values.network.macvlan.create -}}
{{- if not .Values.network.macvlan.master -}}
{{- fail "network.macvlan.master is required when creating a NetworkAttachmentDefinition" -}}
{{- end -}}
{{- if not .Values.network.macvlan.ipam.subnet -}}
{{- fail "network.macvlan.ipam.subnet is required when creating a NetworkAttachmentDefinition" -}}
{{- end -}}
{{- else -}}
{{- if not .Values.network.macvlan.existingName -}}
{{- fail "network.macvlan.existingName is required when create is false" -}}
{{- end -}}
{{- end -}}
{{- end -}}
{{- if and .Values.discovery.enabled (not .Values.discovery.allowedCIDRs) -}}
{{- fail "discovery.enabled is true but discovery.allowedCIDRs is empty; list the networks the scan nodes may probe, or disable discovery" -}}
{{- end -}}
{{- end }}

{{/*
The Multus attachment name for macvlan mode.
*/}}
{{- define "emberwire-app.macvlanNetwork" -}}
{{- if .Values.network.macvlan.create -}}
{{- include "emberwire-app.nadName" . -}}
{{- else -}}
{{- .Values.network.macvlan.existingName -}}
{{- end -}}
{{- end }}

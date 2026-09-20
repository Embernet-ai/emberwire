{{/*
Name.

SYNC: must equal the Service name (.Release.Name). The Industrial Dashboard
reads app.kubernetes.io/name for Rancher proxy routing, and if this returns the
chart name ("hotloop-flow") while the Service is named "hotloop-flow-embernet003",
the proxy 404s.
*/}}
{{- define "hotloop-flow.name" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{/*
Fully qualified name. Forced to .Release.Name so the dashboard can resolve
<release-name>.<namespace>.svc.cluster.local, and so two instances on one node
are distinguishable.
*/}}
{{- define "hotloop-flow.fullname" -}}
{{- .Release.Name | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "hotloop-flow.chart" -}}
{{- printf "%s-%s" .Chart.Name .Chart.Version | replace "+" "_" | trunc 63 | trimSuffix "-" }}
{{- end }}

{{- define "hotloop-flow.labels" -}}
helm.sh/chart: {{ include "hotloop-flow.chart" . }}
{{ include "hotloop-flow.selectorLabels" . }}
{{- if .Chart.AppVersion }}
app.kubernetes.io/version: {{ .Chart.AppVersion | quote }}
{{- end }}
app.kubernetes.io/managed-by: {{ .Release.Service }}
{{- with (include "hotloop-flow.tenantLabels" .) }}
{{ . }}
{{- end }}
{{- end }}

{{/*
Labels the EmberNET dashboard asks for on everything a deploy creates.

On an App Store deploy the dashboard passes tenantLabels (the tenant, the app id,
who deployed it, the deployment id) and expects the chart to fold them onto every
resource it renders. That is how the Running Apps view tells one tenant's app from
another's, and how a deployment is traced back to the record of it. This chart did
not read the value at all until 2.0.3, found by deploying it to a tenant cluster
and looking at the labels on what arrived: none of the five were there.

Values are quoted, because a label value is always a string.
*/}}
{{- define "hotloop-flow.tenantLabels" -}}
{{- range $k, $v := .Values.tenantLabels }}
{{ $k }}: {{ $v | toString | quote }}
{{- end }}
{{- end }}

{{- define "hotloop-flow.selectorLabels" -}}
app.kubernetes.io/name: {{ include "hotloop-flow.name" . }}
app.kubernetes.io/instance: {{ .Release.Name }}
app: {{ include "hotloop-flow.fullname" . }}
{{- end }}

{{/*
EmberNET Store discovery labels — THE BIG FOUR.

On the pod template AND the Service. All four, always. Miss one and the app is
invisible to the dashboard; miss them on the Service specifically and it shows
in node detail but not in Running Apps.
*/}}
{{- define "hotloop-flow.storeLabels" -}}
embernet.ai/store-app: "true"
embernet.ai/gui-type: {{ .Values.gui.type | default "web" | quote }}
embernet.ai/app-name: {{ include "hotloop-flow.name" . | quote }}
embernet.ai/gui-port: {{ .Values.gui.port | default .Values.service.port | quote }}
{{- end }}

{{/*
Display name.

The dashboard injects .Values.embernet.displayName at deploy time and it must
win. Reading .Values.gui.displayName alone silently drops the injected value and
every node card shows the raw release name — the bug fixed in nodered-pod 2.2.4.
*/}}
{{- define "hotloop-flow.storeAnnotations" -}}
{{- $dn := "" -}}
{{- with .Values.embernet }}{{- if .displayName }}{{- $dn = .displayName -}}{{- end }}{{- end }}
{{- if not $dn }}{{- $dn = .Values.gui.displayName -}}{{- end }}
{{- if $dn }}
embernet.ai/display-name: {{ $dn | quote }}
{{- end }}
{{- end }}

{{- define "hotloop-flow.serviceAccountName" -}}
{{- if .Values.serviceAccount.create }}
{{- default (include "hotloop-flow.fullname" .) .Values.serviceAccount.name }}
{{- else }}
{{- default "default" .Values.serviceAccount.name }}
{{- end }}
{{- end }}

{{- define "hotloop-flow.pvcName" -}}
{{- printf "%s-data" (include "hotloop-flow.fullname" .) }}
{{- end }}

{{- define "hotloop-flow.secretName" -}}
{{- printf "%s-auth" (include "hotloop-flow.fullname" .) }}
{{- end }}

{{- define "hotloop-flow.configMapName" -}}
{{- printf "%s-config" (include "hotloop-flow.fullname" .) }}
{{- end }}

{{- define "hotloop-flow.nadName" -}}
{{- printf "%s-macvlan" (include "hotloop-flow.fullname" .) }}
{{- end }}

{{- define "hotloop-flow.resources" -}}
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
{{- define "hotloop-flow.credentialSecret" -}}
{{- if .Values.hotloopFlow.credentialSecret -}}
{{- .Values.hotloopFlow.credentialSecret -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "hotloop-flow.secretName" .) -}}
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
{{- define "hotloop-flow.adminPassword" -}}
{{- if .Values.auth.password -}}
{{- .Values.auth.password -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" .Release.Namespace (include "hotloop-flow.secretName" .) -}}
{{- if and $existing $existing.data (hasKey $existing.data "admin-password") -}}
{{- index $existing.data "admin-password" | b64dec -}}
{{- else -}}
{{- randAlphaNum 20 -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Admin password hash. Called with a dict: the root context and the password the
Secret is being written with.

An explicit hash wins. Otherwise the hash is made from the password the caller
passes in, so the two can never describe different passwords. This used to call
adminPassword a second time, and on a first install, when there is no Secret to
read back, each call drew its own random password. The stored password never
matched the stored hash, so the login the install notes print did not work.

An existing hash is kept, so an upgrade does not rotate it and restart the pod,
but only when the Secret carries the marker annotation and its stored password is
still the one being written. A Secret written before the marker existed has a hash
that may belong to some other password, so it is made again, once, from the stored
password. That is what makes the printed login start working on a release that
was installed with the bug.
*/}}
{{- define "hotloop-flow.adminPasswordHash" -}}
{{- $root := .root -}}
{{- if $root.Values.auth.passwordHash -}}
{{- $root.Values.auth.passwordHash -}}
{{- else -}}
{{- $existing := lookup "v1" "Secret" $root.Release.Namespace (include "hotloop-flow.secretName" $root) -}}
{{- $keep := false -}}
{{- if and $existing $existing.data (hasKey $existing.data "admin-password-hash") (hasKey $existing.data "admin-password") $existing.metadata.annotations -}}
{{- if and (hasKey $existing.metadata.annotations "hotloop-flow.hotloop.io/hash-matches-password") (eq (index $existing.data "admin-password" | b64dec) .password) -}}
{{- $keep = true -}}
{{- end -}}
{{- end -}}
{{- if $keep -}}
{{- index $existing.data "admin-password-hash" | b64dec -}}
{{- else -}}
{{- htpasswd $root.Values.auth.username .password | trimPrefix (printf "%s:" $root.Values.auth.username) -}}
{{- end -}}
{{- end -}}
{{- end }}

{{/*
Validate the network mode and fail the render rather than producing a manifest
that is wrong in a way nobody notices until a pod is Pending.
*/}}
{{- define "hotloop-flow.validateNetwork" -}}
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
{{- define "hotloop-flow.macvlanNetwork" -}}
{{- if .Values.network.macvlan.create -}}
{{- include "hotloop-flow.nadName" . -}}
{{- else -}}
{{- .Values.network.macvlan.existingName -}}
{{- end -}}
{{- end }}

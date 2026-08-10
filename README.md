# Emberwire

**Node-RED's idea. My runtime.** A flow engine in Go for EmberNET: one static
binary, an editor that belongs to us, and a scheduler that does not fall over
when a sensor starts talking faster than the thing reading it.

[![CI](https://github.com/Embernet-ai/emberwire/actions/workflows/ci.yml/badge.svg)](https://github.com/Embernet-ai/emberwire/actions/workflows/ci.yml)
[![Licence](https://img.shields.io/badge/licence-Apache--2.0-blue)](LICENSE)
[![Go](https://img.shields.io/badge/go-1.26-00ADD8)](go.mod)

We shipped Node-RED in our own App Store for a year. It works. That is the
genuinely irritating part, because "it works" is where most people stop looking.
It is 717 MB. It runs one single-threaded event loop for the runtime, the editor,
the websockets, and every node's I/O simultaneously. It looks like somebody
else's product wearing our dashboard as a coat. I sat there at 3 AM reading that
image size off a registry listing, felt something structural give out behind my
eyes, and opened a Go file instead of going to bed. There was a White Monster
involved. There is always a White Monster involved.

This is what came out. **25.1 MB.** Your flows still load.

**Flow files stay Node-RED v1 compatible.** Point it at your `flows.json` and it
runs. Nobody loses a year of work because I had opinions at three in the morning.
Everything else was fair game, and I took nearly all of it.

Published as `v0.1.0`. Image `ghcr.io/embernet-ai/emberwire:0.1.0` for amd64 and
arm64, chart at `https://embernet-ai.github.io/emberwire/`. Roughly 30,000 lines
of Go, 51 node types, and a race detector that comes back clean on every package.

---

## The numbers

Both runtimes in rootless podman on one box, the same five-node flow file
deployed to each **unchanged**, driven by the same load generator in the same
sitting. Linux, 12 CPUs, `nodered/node-red:latest`, 8 connections, 30 seconds.

| | Emberwire | Node-RED | |
|---|---|---|---|
| Image size | **25.1 MB** | 717 MB | 29× |
| Memory, idle | **4.8 MB** | 54.6 MB | 11× |
| Memory, under load | **12.3 MB** | 179.6 MB | 15× |
| Throughput | **3,460 req/s** | 1,290 req/s | 2.7× |
| Latency p50 | **1.03 ms** | 4.61 ms | 4.5× |
| Latency p99 | **16.72 ms** | 23.14 ms | 1.4× |
| Cold start | **2.1–2.3 s** | 4.6–5.5 s | ~2.3× |

Now here is me walking my own numbers back, because a benchmark table with no
caveats under it is marketing wearing a lab coat. Three things those figures are
not allowed to say.

Cold start includes roughly two seconds of container start that both runtimes
pay, so the real gap between the two programs is smaller than the row implies.
The throughput figure includes the whole HTTP stack on purpose, since that is the
only surface both present identically, which means it is not a measurement of
either scheduler in isolation and I will not let anyone quote it as one. And the
p99 gap is far narrower than the p50 gap for a reason that is not flattering to
anybody: under saturation both runtimes queue, queueing dominates the tail, and
the median is the only column where per-request cost actually shows up.

Produced by `emberwire bench`, which is in this repository, so you can go
disagree with me on your own hardware. Method, flow file, exact commands, and
caveats live in [docs/bench/](docs/bench/). **Not one figure here came off a
forum post**, and nothing comparative goes into a document until that command
produced it on one box in one run.

---

## Why I did not just keep running Node-RED

Four things. None of them are fixable with configuration, which is the only
reason I wrote thirty thousand lines of Go instead of a values file.

**There is no back-pressure.** Node-RED puts a `setImmediate` between every wire
hop and that queue has no ceiling. A fast source outruns a slow sink, the queue
grows, and the pod gets OOM-killed with nothing in the log explaining itself.
[node-red#855](https://github.com/node-red/node-red/issues/855) has been open
since 2016. Every inbox here is bounded, with a policy per node: block, drop the
newest, drop the oldest, or raise it to a Catch node and let the flow decide what
it wants to do about it.

**It is single-threaded.** One event loop carries the runtime, the editor API,
the websocket fan-out, and every node's I/O. One CPU-heavy Function node stalls
all of it, including the editor you are using to find out why it stalled. Here
every node instance gets its own goroutine. Ordered within a node, parallel
across nodes.

**The Function node's sandbox is not a boundary.** This is not my accusation, it
is Node's own documentation: [*"The `node:vm` module is not a security mechanism.
Do not use it to run untrusted code."*](https://nodejs.org/api/vm.html)
Node-RED's actual trust model is that anyone who can deploy a flow already owns
the box, and honestly, fair, the `exec` node is right there in the palette. That
model is fine on a Pi in a workshop. It is not fine on a customer's plant floor,
and it is not hypothetical either:
[CVE-2025-41656](https://nvd.nist.gov/vuln/detail/CVE-2025-41656) is
unauthenticated remote code execution against a default Node-RED, reached by
deploying a flow with an `exec` node in it.

**Credentials are encrypted with AES-256-CTR keyed by a raw SHA-256 of your
secret.** CTR has no MAC, so anyone who can write `flows_cred.json` on a shared
PVC can flip chosen plaintext bits, which turns "can write a file" into "can
change a broker password to one they picked". And plain SHA-256 is not a KDF. It
is fast by design, which is precisely backwards for the one thing standing
between a stolen file and a weak passphrase. There is no CVE on either of those.
Both are still real.

---

## What is different, concretely

| | Node-RED | Emberwire |
|---|---|---|
| Runtime | Node.js, one event loop | Go, goroutine per node |
| Back-pressure | none, unbounded queue | bounded inbox, four policies |
| Message cloning | first recipient aliases the sender | every recipient gets a copy |
| Function sandbox | `node:vm`, explicitly not a boundary | goja with no host bindings, plus WASM guests with a hard memory ceiling |
| `exec` node | any command, through a shell | disabled until allowlisted, and no shell at all |
| File nodes | any path the process can reach | scoped to the PVC, symlinks resolved |
| Credentials | AES-256-CTR, SHA-256 as the key | AES-256-GCM, Argon2id |
| Context API | get, set | get, set, **CompareAndSwap, Increment, Update** |
| Flow file writes | in place | temp, fsync, rename, fsync dir, three backups deep |
| Config | `settings.js`, executable JavaScript | declarative YAML |
| Auth | off by default | refuses to start without it |
| Metrics | none | Prometheus, per node |
| Editor | ~40k lines of jQuery and D3 | vanilla TypeScript, native SVG, our theme, 34 kb of JS and 12 kb of CSS |
| Node definition | a `.js` plus a hand-written `.html` twin | one Go descriptor |

### Everything unbounded over there is bounded here, visibly

This is the throughline of the entire project, so I am putting it in its own
table. Node-RED's characteristic failure mode is a pod that quietly inflates
until the kubelet kills it, leaving a log that explains nothing to the person
holding the pager.

| | Node-RED | Emberwire |
|---|---|---|
| Node inbox | unbounded | bounded, four overflow policies |
| Delay and rate-limit queue | unbounded | bounded, refused to a Catch node past the limit |
| Trigger timers | unbounded | bounded |
| `exec` output | buffered without limit | capped per stream, truncation raises an error |
| `exec` concurrency | one process per message | bounded, refused past the limit |
| File read | whole file into memory | capped, with per-line and chunked modes offered |
| HTTP request body | unbounded | capped |
| HTTP response wait | forever | 504 after a timeout |
| TCP connections and frame size | unbounded | bounded |
| WebSocket send queue | unbounded | bounded, slow client disconnected |

Every one of those is a written, documented divergence in
[docs/compatibility.md](docs/compatibility.md) rather than a silent cap I decided
on and never mentioned. And nothing is thrown away quietly: every dropped message
is counted, announced, and exported as a metric you can alert on.

### Cloning, and why I broke compatibility on purpose

Node-RED hands the **first** recipient on a wire the original message object and
only clones for the recipients after it. It is a documented memory optimisation.
It is also why two branches that each believe they own their message can quietly
corrupt each other, and why the last branch you happened to wire is the one that
behaves differently from its siblings. That is a bug that reproduces on Tuesdays.

Every recipient here gets its own copy. That costs 1.4 µs per hop on my box,
which is essentially the entire per-hop cost, since a five-node chain runs 6.8 µs
end to end. I bought most of it back with `ImmutableBytes`, a
shared-rather-than-copied path for large binary payloads: **341 ns instead of
217 µs on a 1 MB payload, and 360 bytes allocated instead of a megabyte.** That
is exactly where the copying would have hurt, and it is measured rather than
assumed.

### Atomic context operations

Node-RED's context API is get and set. Those two verbs are the specific reason a
Node-RED flow [cannot be run in more than one
instance](https://flowfuse.com/blog/2023/05/bringing-high-availability-to-node-red/):
two copies doing get-modify-set against a shared counter will race, and the API
offers no primitive you could fix it with even if you noticed. `CompareAndSwap`,
`Increment`, and `Update` cost nothing on a transactional store. Tested under
10,000 concurrent increments with zero lost updates.

---

## 🔒 Security posture

Not a feature list. These are the four places where "anyone who can edit a flow"
stops being a synonym for "anyone who owns the box".

**It refuses to start without authentication.** Not a warning in a log nobody
reads, not a default you are trusted to change, but a startup error with the
remedy printed next to it. Node-RED shipping unauthenticated by default is the
root cause of CVE-2025-41656, and Node-RED's own team proposed this exact fix in
designs#81 and did not ship it.

**The `exec` node ships disabled.** An operator names the commands a flow is
permitted to run, and an enabled node with an empty allowlist is a configuration
error rather than a licence to run anything at all. The allowlist matches on the
**resolved absolute path**, not the string the flow typed, because comparing
strings would let a flow ask for `curl` and receive whichever `curl` sits first
on a `PATH` that a Function node can read and a sidecar can influence. There is
also no shell: the command line is split on quoting rules only, and an unquoted
metacharacter is refused rather than passed through as a literal and hoped about.

**The file nodes are scoped to the PVC.** Symlinks are resolved over the longest
existing prefix of the path, which closes the obvious hole, being: write a file
under the PVC, symlink it to `/`, then read straight through the link. A textual
prefix check waves that through without blinking.

**Credentials are AES-256-GCM with Argon2id**, and the flow file is written temp,
fsync, rename, fsync the directory, three backups deep, recovering from a backup
and saying so out loud if it ever reads a corrupt one.

The discovery nodes get the same treatment: off by default, bounded by an
operator-configured CIDR allowlist, and a hostname that resolves to one in-scope
address and one out-of-scope address is refused outright, because otherwise DNS
is simply the way around the allowlist.

---

## The palette

**51 node types.** Everything Node-RED ships that is not a community node.

| Category | Nodes |
|---|---|
| Common | inject, debug, complete, catch, status, link in, link out, comment, junction |
| Function | function, switch, change, range, template, delay, trigger, exec, rbe |
| Network | mqtt in/out, http in, http response, http request, websocket in/out, tcp in/out, tcp request, udp in/out |
| Sequence | split, join, sort, batch |
| Parser | csv, html, json, xml, yaml |
| Storage | file, file in, watch, influxdb out, postgres |
| Discover | scan, netinfo, both ours, for inventorying an OT segment |
| Config | mqtt-broker, websocket-listener, websocket-client, influxdb, postgres |

By compatibility level: 10 full, 26 partial, 9 deliberately divergent, and 6 with
no Node-RED counterpart at all. Every node declares how it relates to its
Node-RED equivalent, and **a test fails the build if a node claims partial
compatibility without stating in writing what is missing.** A node that is 90%
compatible and silent about the other 10% is more dangerous than one that is
obviously absent, because the first one lets a flow appear to work. Full matrix:
[docs/compatibility.md](docs/compatibility.md), which is generated from the
registry rather than maintained by hand, because a hand-maintained compatibility
document is a lie with a timestamp.

Two nodes are ours. `scan` sweeps a CIDR range and identifies Modbus and
EtherNet/IP endpoints, and `netinfo` reports the interfaces the runtime can
actually see, which in macvlan mode is how a flow discovers its own address on
the OT VLAN.

**Node-RED community nodes do not work here.** They are npm packages that need
Node.js. There is no future version of this where they suddenly do, and I am not
going to imply otherwise to make the table look nicer. That is the trade you make
for the footprint and the sandbox, stated plainly so you can decide against it.

---

## Two sandboxes, and why there are two

The Function node runs on [goja](https://github.com/dop251/goja), a JavaScript
interpreter in pure Go with no host bindings unless somebody adds them, and
nobody added them. A call costs about 21 µs. It is a real boundary in the way
`node:vm` is documented not to be.

It is also not a boundary against memory. A JavaScript function that allocates in
a loop grows the Go heap until the pod dies, and the only defence goja can offer
is a wall-clock timeout. So there is a second option: WebAssembly guests on
[wazero](https://wazero.io/), where linear memory has a declared maximum and a
guest that allocates past its ceiling gets a trap while the host carries on
unbothered. A WASM call costs about 169 µs, which is eight times a goja call and
worth it precisely when you are running something you do not fully trust.

It also means a node can be written in Rust, TinyGo, Zig, or AssemblyScript
instead of JavaScript, which matters for the signal processing an OT flow
actually wants to do.

The guest ABI is deliberately tiny, three exports, two of which are allocator
hooks. A wide ABI is a wide attack surface and one more thing every guest author
has to get right.

| Export | Signature | Required |
|---|---|---|
| `emberwire_process` | `(ptr i32, len i32) -> i64` | yes |
| `emberwire_alloc` | `(size i32) -> i32` | yes |
| `emberwire_free` | `(ptr i32, size i32)` | no |

The `i64` result packs an offset in the high 32 bits and a length in the low 32,
pointing at a JSON response in the guest's own memory. Everything travels as JSON
so a guest in any language can produce it without a shared schema compiler.

Limits, small on purpose, because this runs on edge hardware next to the flows
that actually matter and a node that needs more should have to say so:

| | goja | WASM |
|---|---|---|
| Timeout per call | 5 s | 5 s |
| Memory ceiling | none enforceable | 64 MiB, hard |
| Max output | 16 MiB | 8 MiB |

Both runtimes are pure Go with no cgo, which is what keeps `CGO_ENABLED=0` and
the distroless image true. That constraint is load-bearing, not aesthetic.

---

## Subflows

Each instance gets its own copy of the template's nodes, its own flow context,
and its own resolved properties. The same template renders `4.2 bar` in one
instance and `4.2 kPa` in the next, verified end to end through a real deploy
rather than asserted in a unit test.

Expansion produces a **separate graph** rather than rewriting the parsed flows,
and that is the whole design decision. The flow file is never touched, so the
byte-identical round-trip below still holds, and the expanded graph is an
ordinary graph, so the scheduler, the Catch routing, the metrics, and the
editor's status events all work inside a subflow with no special cases anywhere.

Nesting works. An error nobody catches inside a subflow walks out to the Catch
node on the calling tab, because otherwise a subflow is just a place where errors
go to disappear. A configuration node declared inside a template is **shared** by
every instance rather than copied, since an MQTT broker inside a subflow is an
author saying "share this", and copying it would open one connection per instance
against something that is almost certainly counting them.

---

## Compatibility

`flows.json` v1 loads and saves **byte for byte**. Load a file Node-RED wrote,
save it without editing anything, and you get identical bytes back: same key
order, same spacing, no helpfully rewritten escapes. Edit one property and the
diff is one line rather than the entire node.

That is harder in Go than it sounds and I very nearly did not bother. A
JavaScript object preserves key insertion order, so Node-RED gets this for free
from `JSON.parse` and `JSON.stringify` without anybody thinking about it. A Go
map has no order at all and `encoding/json` deliberately sorts keys. Rather than
force an order-preserving map through every read path in the codebase, each
entry's original bytes are kept and re-emitted when the parsed form is unchanged,
then re-encoded against the original key order when it is not. `json.Compact` and
`json.Indent` are byte-level transforms, so they re-indent without reordering
anything.

It matters for two reasons, both boring and both real. Your flow file lives on a
PVC and in git, and it should not churn just because a pod restarted. And when an
operator reviews a deploy diff before pushing it to a line, they should see what
changed and absolutely nothing else.

A node type this build has never heard of survives a load-and-save with every
property intact, so it is safe to run a Node-RED-authored flow here and hand it
back afterwards. Node-RED's `flows_cred.json` imports read-only, and anything
read that way is re-encrypted under GCM on the next save.

Before you deploy anything, ask it what is going to happen:

```bash
emberwire import flows.json
```

It reports every node type in the file, including the ones inside subflows, split
into supported, partially supported with the gap spelled out, and not supported
at all. Subflow internals are counted against the expanded graph rather than the
file, so an instance never gets reported as "supported" while staying silent
about what is inside it. That is the difference between finding out now and
finding out when a line stops.

---

## Quick start

From source:

```bash
cd web && npm install && npm run build && cd ..
go build -o emberwire ./cmd/emberwire

export EMBERWIRE_DATA_DIR=./data
export EMBERWIRE_ADMIN_USER=admin
export EMBERWIRE_ADMIN_PASSWORD_HASH="$(./emberwire hash-password -password 'something-long')"
export EMBERWIRE_CREDENTIAL_SECRET="$(openssl rand -hex 32)"

./emberwire
```

The editor build comes first and is not optional. The bundle is embedded with
`go:embed`, so a Go build without it fails on a missing pattern rather than
producing a binary with no editor in it.

Or skip all of that:

```bash
docker run --rm -p 1880:1880 -v emberwire-data:/data \
  -e EMBERWIRE_ADMIN_USER=admin \
  -e EMBERWIRE_ADMIN_PASSWORD_HASH='<bcrypt hash>' \
  -e EMBERWIRE_CREDENTIAL_SECRET="$(openssl rand -hex 32)" \
  ghcr.io/embernet-ai/emberwire:0.1.0
```

Generate the hash with `docker run --rm ghcr.io/embernet-ai/emberwire:0.1.0
hash-password -password 'something-long'`. The image is distroless nonroot with
no shell in it, so there is nothing to `exec` into and nothing for anybody who
gets code execution to pivot with.

Then open <http://localhost:1880>. Drop a `flows.json` into the data directory
and restart, or just build the flow in the editor.

### The commands

| Command | What it does |
|---|---|
| `emberwire` | Serves the runtime, the admin API, and the editor. The default. |
| `emberwire -config <path>` | Same, from a YAML file. Also reads `EMBERWIRE_CONFIG`. |
| `emberwire hash-password` | bcrypt hash for a password. Takes `-password` or `EMBERWIRE_PASSWORD`. Refuses anything under 8 characters. |
| `emberwire import <flows.json>` | Reports what would happen before you deploy it. |
| `emberwire bench` | The benchmark harness that produced the table above. |
| `emberwire version` | The version. |

---

## Configuration

Declarative YAML with environment overrides, because `settings.js` is executable
JavaScript in which `adminAuth` can be a function, `https` can be a function
returning cert options, and `storageModule` can be a `require()`. You cannot
validate that, diff it, template it out of a ConfigMap, or reason about it
without running it first. Every field below is optional and shown at its default.

```yaml
server:
  host: 0.0.0.0
  port: 1880
  adminRoot: /              # where the editor and admin API live
  httpRoot: /               # where a flow's HTTP In nodes live
  readTimeout: 60s          # generous: a big deploy over a slow edge link is legitimate
  writeTimeout: 0s          # zero on purpose, the comms websocket is long-lived
  shutdownTimeout: 20s
  maxRequestBytes: 33554432 # 32 MiB, bounds a flow deploy

data:
  dir: /data                # the PVC. Flows, credentials, and context all live here
  flowFile: flows.json
  credentialsFile: credentials.json
  credentialSecret: ""      # empty means plaintext, which is refused by default
  allowPlaintextCredentials: false
  backupGenerations: 3

auth:
  enabled: true             # it will not start with this off unless you say so twice
  sessionTTL: 168h
  users:
    - username: admin
      passwordHash: "$2a$10$..."   # bcrypt only, never plaintext
      permissions: ["*"]

runtime:
  inboxCapacity: 1024
  overflow: block           # block | drop-newest | drop-oldest | error
  blockTimeout: 30s
  closeTimeout: 15s

discovery:
  enabled: false
  allowedCIDRs: []          # enabled with this empty is a startup error

exec:
  enabled: false
  allowedCommands: []       # enabled with this empty is a startup error

files:
  allowedPaths: []          # extra trees on top of data.dir, which is always allowed

logging:
  level: info               # error | warn | info | debug | trace
  format: text              # text for a terminal, json for a cluster

metrics:
  enabled: true
  path: /metrics
```

A typo in that file is a startup failure rather than a setting that silently does
nothing, because unknown fields are rejected. An operator who misspells
`credentialSecret` should find out immediately, not six months later when they
notice the credential file is plaintext.

### The overflow policies

`runtime.overflow` sets the default and any node can override it. This is the
setting that does not exist over there at all.

| Policy | Behaviour |
|---|---|
| `block` | The sender waits for space, up to `blockTimeout`. Back-pressure propagates upstream, which is the correct answer almost always. |
| `drop-newest` | Discard the arriving message. Counted and announced. |
| `drop-oldest` | Discard the head of the queue to make room. Counted and announced. For "only the latest reading matters" flows. |
| `error` | Refuse the send and raise it to a Catch node, letting the flow decide. |

### Environment overrides

Environment beats the file, which is the only workable split on Kubernetes: a
Helm chart puts the boring settings in a ConfigMap and injects the secrets from a
Secret.

| Variable | Sets |
|---|---|
| `EMBERWIRE_CONFIG` | Path to the YAML file. |
| `EMBERWIRE_HOST`, `EMBERWIRE_PORT` | Listener. |
| `EMBERWIRE_ADMIN_ROOT`, `EMBERWIRE_HTTP_ROOT` | Path prefixes. |
| `EMBERWIRE_DATA_DIR`, `EMBERWIRE_FLOW_FILE` | Where state lives. |
| `EMBERWIRE_CREDENTIAL_SECRET` | Credential encryption secret. |
| `EMBERWIRE_ADMIN_USER`, `EMBERWIRE_ADMIN_PASSWORD_HASH` | A single admin account with full permissions, which is what makes a first-run container usable without mounting a file. Both must be set. |
| `EMBERWIRE_INBOX_CAPACITY`, `EMBERWIRE_OVERFLOW` | Scheduler defaults. |
| `EMBERWIRE_LOG_LEVEL`, `EMBERWIRE_LOG_FORMAT` | Logging. |
| `EMBERWIRE_DISCOVERY_ENABLED`, `EMBERWIRE_DISCOVERY_CIDRS` | Discovery nodes. Comma-separated CIDRs. |
| `EMBERWIRE_EXEC_ENABLED`, `EMBERWIRE_EXEC_ALLOWED_COMMANDS` | The exec node. Comma-separated commands. |
| `EMBERWIRE_FILE_ALLOWED_PATHS` | Extra file node roots. Comma-separated. |
| `EMBERWIRE_INSECURE` | Disables authentication. Do not. |
| `EMBERWIRE_ALLOW_PLAINTEXT_CREDENTIALS` | Permits unencrypted credentials at rest. |

The two dangerous ones accept only `1`, `true`, `yes`, and `on`. Setting
`EMBERWIRE_INSECURE=0` or `=false` does not disable authentication, and neither
does anything else you can think of, which is the entire point of writing that
parser by hand rather than reaching for `strconv.ParseBool`.

Anything not in that table is file-only, and that is deliberate: session TTL,
backup generations, request size limits, and the server timeouts are decisions
somebody should be reviewing in a ConfigMap, not typing into a shell at 3 AM
while a line is down. I have been that person. Do not let that person edit
timeouts.

---

## The admin API

Same shape as Node-RED's where a client already expects it, with real permissions
on top.

| Method and path | Permission | Notes |
|---|---|---|
| `GET /health` | none | Liveness. The kubelet carries no token, and auth on this route would restart-loop the pod forever. |
| `GET /ready` | none | Readiness, reported separately, so a runtime that failed to start leaves the Service without the kubelet killing the pod. |
| `POST /auth/token` | none | Log in. |
| `POST /auth/revoke` | none | Log out. |
| `GET /metrics` | none | Prometheus. Counts and node ids only, never message contents or configuration. |
| `GET /settings` | `settings.read` | |
| `GET /nodes` | `nodes.read` | The registry, which is what drives the editor's palette and its dialogs. |
| `GET /flows` | `flows.read` | |
| `POST /flows` | `flows.write` | Deploy. |
| `GET /runtime/stats` | `status.read` | |
| `POST /inject/{id}` | `inject.write` | Fire an Inject node. |
| `GET /comms` | `status.read` | The editor's status and debug websocket. |

`/metrics` and `/health` being unauthenticated is a decision rather than an
oversight. A Prometheus scraper carries no bearer token, so requiring one means
either handing a credential to your monitoring stack or having no monitoring, and
I have watched people pick the second one.

Permissions are `"*"` for everything, an exact string such as `flows.read`, or a
prefix grant such as `flows.*`. A read-only account for a dashboard that just
wants to render flow status is `["flows.read", "status.read"]` and nothing more.

Deploys take an optional `Emberwire-Deployment-Rev` header. Send the revision you
last read and a deploy racing another editor is rejected instead of silently
overwriting somebody's work, which is the failure mode you only find out about
from whoever lost their afternoon.

---

## Metrics

Prometheus at `/metrics`, per node, with `node` and `type` labels on everything
per-node. No exporter sidecar, because an edge box does not have room for one and
you should not need a second container to find out that your inbox is full.

| Metric | Type | What it tells you |
|---|---|---|
| `emberwire_build_info` | gauge | Always 1. The version rides in the label. |
| `emberwire_uptime_seconds` | gauge | Seconds since the runtime started. |
| `emberwire_nodes_running` | gauge | Node instances currently running. |
| `emberwire_node_messages_received_total` | counter | Messages delivered to a node. |
| `emberwire_node_messages_sent_total` | counter | Messages a node has emitted. |
| `emberwire_node_errors_total` | counter | Errors a node raised. |
| `emberwire_node_messages_dropped_total` | counter | Messages discarded because an inbox was full. Node-RED cannot report this, because it has no bound to overflow. |
| `emberwire_node_sends_blocked_total` | counter | Times a sender waited for space. Sustained back-pressure, which is the real signal that a flow cannot keep up. |
| `emberwire_node_queue_length` | gauge | Messages waiting right now. |
| `emberwire_node_queue_capacity` | gauge | Where the overflow policy starts applying. |
| `emberwire_node_queue_high_water` | gauge | The deepest that inbox has ever been. |
| `emberwire_goroutines` | gauge | Roughly one per node plus the I/O each holds. |
| `emberwire_memory_heap_bytes` | gauge | Heap currently allocated. |
| `emberwire_memory_sys_bytes` | gauge | Bytes taken from the OS. |
| `emberwire_gc_cycles_total` | counter | Completed GC cycles. |

The one to alert on is `emberwire_node_queue_high_water` against
`emberwire_node_queue_capacity`. High water is the early warning that a flow is
approaching its ceiling, which arrives before anything is dropped and long before
anyone is awake. That alert is the whole reason I built the bounded inbox, and it
is the metric Node-RED structurally cannot give you: you cannot report how close
you are to a limit that does not exist.

---

## Deploying on EmberNET

```bash
helm repo add emberwire https://embernet-ai.github.io/emberwire/
helm install line3-flows emberwire/emberwire-app
```

Or install it from the App Store in the dashboard, which is the entire point of
it existing. Multi-instance in exactly the way Node-RED is: as many per node as
you need.

One clarification, since it bites people. Multi-instance means multiple
*releases*, not multiple replicas, and the chart pins `replicaCount: 1` on
purpose. Emberwire holds flow state and open connections to brokers and PLCs, so
two pods behind one Service would both subscribe and both write, and your
InfluxDB would quietly receive everything twice. Scaling out means a second
instance with its own flows.

Resources are presets rather than raw numbers, matching the node-red chart so
nobody has to think while switching between them. Default is `small`, requesting
10m CPU and 64Mi of memory, against the node-red chart's default of 512Mi
requested and 2Gi limited. Idle measured 4.8 MB. The can of White Monster on my
desk has more mass than this thing's idle heap, and I am fully aware how
insufferable that is to point out. I am pointing it out anyway.

Three network modes, and this is the reason it earns a place on a plant floor at
all, because a flow engine that can only see what k3s routes to it cannot
inventory an OT segment:

| Mode | What it gets | Instances per node |
|---|---|---|
| `cluster` | ClusterIP. Whatever the cluster routes. The default, and the right answer unless you specifically need L2. | unlimited |
| `host` | The host's interfaces, ARP table, and broadcast domain. | one, the port is the node's |
| `macvlan` | Its **own MAC and IP** on the target VLAN, sitting directly on the segment with the PLCs. | unlimited |

`macvlan` is the interesting one: multi-instance safe **and** on the OT segment,
with no port collisions whatsoever because every instance carries its own
address. It needs Multus on the cluster. No other App Store chart ships this yet.

The pod runs distroless nonroot as uid 65532 with a read-only root filesystem, no
privilege escalation, and `RuntimeDefault` seccomp. The discovery nodes want
`NET_RAW` and `NET_ADMIN` for raw sockets, and without them they fall back to TCP
connect probing rather than failing, so you can leave them off and still get an
inventory.

The chart generates the admin password and the credential secret on first install
and **reads both back off the existing Secret on upgrade**. That is not
defensiveness for its own sake. If the credential secret ever regenerates, every
credential already encrypted onto that PVC becomes undecryptable, every broker
password in every flow is simply gone, and it presents as a completely clean
upgrade until somebody notices that MQTT will not authenticate. Enable the
optional ServiceMonitor and you will at least be watching when it happens.

---

## ⚠️ When it refuses to start

It refuses to start for five reasons, and every one of them prints the remedy
rather than a stack trace, because the person reading that output is looking at a
CrashLoopBackOff and does not need my Go paths.

| Refusal | Fix |
|---|---|
| Authentication is disabled | Configure a user. Or set `EMBERWIRE_INSECURE=true` if the network is genuinely isolated and you have decided to own that. |
| Authentication is on with no users | Set `auth.users`, or `EMBERWIRE_ADMIN_USER` and `EMBERWIRE_ADMIN_PASSWORD_HASH`. |
| A `passwordHash` that is not bcrypt | Run `emberwire hash-password`. This check exists so a plaintext password can never end up in a ConfigMap by accident. |
| No credential secret | Set `EMBERWIRE_CREDENTIAL_SECRET`. Or `EMBERWIRE_ALLOW_PLAINTEXT_CREDENTIALS=true` if this instance holds no secrets at all. |
| `discovery` or `exec` enabled with an empty allowlist | List what is permitted, or turn the thing off. An empty allowlist read permissively is exactly how a narrow capability becomes a shell. |

What it will *not* refuse to start over is one bad node. A flow with a single
broken node runs every other node and logs the failure, because taking a line
down over one typo in one dialog is a worse outcome than running 40 nodes out of
41 and saying so. It also logs warnings rather than dying when the exec allowlist
names something not yet on the `PATH`, then resolves it again later, since an
init container or a mounted volume can legitimately supply it after start-up.

If the flow file is unparseable it recovers from a backup, keeps the corrupt file
next to it as `.corrupt`, and logs an error saying exactly which backup it fell
back to. Deploys write the flow file *before* stopping the old runtime, so a
failure to persist leaves your previous flows running instead of taking the line
down for a bad save. That ordering is not the obvious one and it is not an
accident.

---

## 🚧 Where it stands

Being straight about this, the same way the EmberRTOS README is.

**It runs.** Starts, serves the API and the editor, loads flows off the PVC,
moves messages, and survives restart. The chart deploys it three ways and the
whole publish chain resolves.

**Verified against real infrastructure**, not merely against my own encoders:
MQTT, InfluxDB 2.7, and PostgreSQL 16 in podman. The InfluxDB tag value comes
back out of a real database as `press 01,west` with the space and the comma
intact, which is the escaping bug that would otherwise fragment a series in
silence and cost somebody a day.

**Race detector clean**, every package, on Linux with cgo.

**Not done yet**, roughly in the order it bothers me:

- **Partial deploy.** A redeploy currently restarts every node rather than only
  the ones that changed. Node-RED diffs. This is the last big runtime gap.
- **Link Call, and Link Out's "return" mode.** Both refused with an error rather
  than silently doing nothing, which is the right behaviour while they do not
  exist, and still a gap.
- **JSONata.** Every `jsonata`-typed property is refused today. Refusing beats
  returning the expression text and letting a flow route on a literal string, but
  it is also the single most common thing an imported flow trips over.
- **Cron-style Inject scheduling.** Interval and startup injection work. "At a
  specific time, on these days" does not, and `crontab` is ignored.
- **Editor click-through for the newer nodes.** The dialogs are descriptor-driven
  so they render, but nothing has been clicked through by hand for the HTTP,
  WebSocket, TCP, or UDP nodes.
- **Multipart uploads on HTTP In**, and cookies on HTTP Response.
- **It has never run on a real plant floor.** Shepherd Boy Farms is the intended
  first site. Everything past the deploy line is unproven in the field, and I am
  not going to describe it as production-hardened until a plant has tried to
  break it.

---

## Layout

```
emberwire/
  cmd/emberwire/     the binary, the import checker, and the benchmark harness
  internal/
    engine/          messages, property expressions, the v1 graph, subflow expansion
    node/            what a node type is: Descriptor, registry, contracts
    nodes/           the built-in palette
    runtime/         the scheduler, delivery, error and status routing
    store/           context, flow file, credentials
    js/              goja host
    wasmhost/        wazero host
    api/             admin REST and the editor websocket
    config/          the YAML surface and the refusals
    metrics/         the Prometheus exposition
    flowhttp/        the route table a flow's HTTP nodes register into
    shell/           the exec node's command allowlist
    filescope/       the file nodes' path scope
    discover/        network discovery
  web/               the editor
  charts/            the App Store chart
  docs/bench/        the benchmark method and results
```

## Building

```bash
cd web && npm install && npm run build && cd ..
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o emberwire ./cmd/emberwire
```

Static, no cgo, runs on distroless. goja and wazero are both pure Go, which is
the only reason that sentence is true. CI fails the build if the binary exceeds
40MiB or turns out to be dynamically linked, because both of those are things you
discover on a plant floor otherwise.

The race detector needs cgo, so run it where a C compiler exists:

```bash
CGO_ENABLED=1 go test -race -count=1 -timeout 900s ./...
```

`docs/compatibility.md` is generated and a test fails when it drifts. Regenerate
it rather than editing it:

```bash
EMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/
```

Integration tests are skipped unless the environment points at a real broker and
a real database. The exact variables are in the header of
`internal/nodes/integration_test.go`, and the point of them is that "it works
against my own encoder" is not a claim worth making.

Run `gofmt -w ./internal ./cmd ./web` before committing. CI does not check
formatting, so that one is on you.

What CI does check, beyond vet and the race detector, is the set of things I have
personally been burned by: that `go mod tidy` is committed, that the sandbox
tests genuinely ran rather than silently skipping, that the editor bundle is
actually inside the binary, that the binary is under 40MiB and statically linked,
and a 60-second fuzz run against the property expression parser. Every one of
those exists because the alternative was a green checkmark over something broken,
and a green checkmark is worse than a red one.

Images and charts publish from **version tags only**, and both workflows refuse
anything else twice: once in the trigger, once in a step that re-checks the ref.
An artefact built from an untagged commit is one nobody can name out loud, and
something on a plant floor can end up running it. Pushing to `main` runs CI and
publishes nothing.

## Adding a node

One Go descriptor per node type, not a `.js` and a hand-written `.html` twin that
drift apart the moment somebody is in a hurry. The descriptor declares the
properties, the ports, the defaults, and the compatibility level, and the editor
renders the dialog from it, so a node cannot ship with a UI that disagrees with
its runtime.

Two rules that are enforced rather than requested. Anything less than fully
compatible must state in writing what is missing, and a test fails the build if
it does not. And nothing may silently discard data: if your node drops something,
count it, announce it, and let it show up in the metrics above. Every place where
this codebase deliberately diverges from Node-RED has a comment explaining why,
not what. Keep that up.

## Licence

Apache 2.0. Emberwire is an independent implementation and contains no Node-RED
source. See [NOTICE](NOTICE) for the attribution and the full list of deliberate
divergences.

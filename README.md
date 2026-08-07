# Emberwire

Node-RED's idea, our runtime. A flow engine in Go for EmberNET — one static
binary, an editor that looks like it belongs to us, and a scheduler that does not
fall over when a sensor decides to talk faster than the thing reading it.

We already ship Node-RED in the App Store. It works. It is also a 450MB image
running a single-threaded event loop, and it looks like somebody else's product
bolted into our dashboard. This replaces it.

**Flow files stay Node-RED v1 compatible.** Import your `flows.json`, it runs.
Nobody loses work. Everything else was up for grabs and I took most of it.

## Why not just keep running Node-RED

Four things, and none of them are fixable with configuration.

**It has no back-pressure.** Node-RED puts a `setImmediate` between every wire
hop and that queue is unbounded. A fast source outruns a slow sink until the pod
gets OOM-killed. [node-red#855](https://github.com/node-red/node-red/issues/855)
has been open since 2016. Every inbox here is bounded, with a policy per node —
block, drop the newest, drop the oldest, or raise it to a Catch node and let the
flow decide.

**It is single-threaded.** One event loop for the runtime, the editor API, the
websocket fan-out and every node's I/O. One CPU-heavy function stalls all of it.
Here every node instance gets its own goroutine. Ordered within a node, parallel
across nodes.

**The Function node's sandbox is not a boundary.** Node's own docs say it:
[*"The `node:vm` module is not a security mechanism. Do not use it to run
untrusted code."*](https://nodejs.org/api/vm.html) Node-RED's trust model is that
anyone who can deploy a flow already owns the box — the `exec` node is right
there. Fair enough, but we can do better than that on a customer's plant floor.

**Credentials are encrypted with AES-256-CTR and a raw SHA-256 of your secret.**
CTR has no MAC, so anyone who can write `flows_cred.json` on a shared PVC can
flip chosen plaintext bits — that turns "can write a file" into "can change a
broker password to one they know". And plain SHA-256 is not a KDF; it is fast by
design, which is exactly backwards for the thing standing between a stolen file
and a weak passphrase. No CVE on either. Both are real.

## What is different, concretely

| | Node-RED | Emberwire |
|---|---|---|
| Runtime | Node.js, one event loop | Go, goroutine per node |
| Back-pressure | none, unbounded queue | bounded inbox, four policies |
| Message cloning | first recipient aliases the sender | every recipient gets a copy |
| Function sandbox | `node:vm`, explicitly not a boundary | goja (no host bindings), plus WASM guests with a hard memory ceiling |
| Credentials | AES-256-CTR, SHA-256 as the key | AES-256-GCM, Argon2id |
| Context API | get, set | get, set, **CompareAndSwap, Increment, Update** |
| Flow file writes | in place | temp, fsync, rename, fsync dir, three backups deep |
| Config | `settings.js`, executable JavaScript | declarative YAML |
| Auth | off by default | refuses to start without it |
| Metrics | none | Prometheus, per node |
| Editor | ~40k lines of jQuery and D3 | vanilla TypeScript, native SVG, our theme |
| Node definition | a `.js` and a hand-written `.html` twin | one Go descriptor |

### Cloning, and why I broke compatibility on purpose

Node-RED hands the **first** recipient on a wire the original message object and
clones only for the ones after it. It is a documented memory optimisation. It is
also why two branches that each think they own their message can quietly step on
each other, and the last branch you wired is the one that behaves differently.

Every recipient here gets its own copy. That costs 1.4µs per hop on my box, which
is basically the entire per-hop cost — a five-node chain runs 6.8µs end to end.
I bought most of it back with `ImmutableBytes`, a shared-not-copied path for
large binary payloads: **341ns instead of 217µs on a 1MB payload, 360 bytes
allocated instead of a megabyte.** That is where the copies would actually have
hurt, and it is measured, not guessed.

### Atomic context operations

Node-RED's context API is get and set. That is the specific reason a Node-RED
flow [cannot be run in more than one
instance](https://flowfuse.com/blog/2023/05/bringing-high-availability-to-node-red/)
— two copies doing get-modify-set on a shared counter race, and there is no
primitive available to fix it with. `CompareAndSwap`, `Increment` and `Update`
cost nothing on a transactional store. Tested under 10,000 concurrent increments
with zero lost updates.

## Where it stands

Being straight about this, the way the EmberRTOS README is.

**Runs.** `emberwire` starts, serves the API and the editor, loads flows off the
PVC and moves messages. The chart deploys it three ways. 30 node types.

**Done and tested.** The engine — message model, property expressions, the v1
flow parser, the scheduler with back-pressure, Catch/Status/Complete with the
group-distance rule, context stores, survivable storage. The admin API, the
websocket, and the binary. The editor: canvas, wiring, descriptor-driven edit
dialogs, deploy. Both function hosts. Database export, MQTT, parsers, network
discovery. The Helm chart with all three network modes, the Dockerfile and the
publish workflows.

**Verified against real infrastructure**, not just against my own encoders:
MQTT, InfluxDB 2.7 and PostgreSQL 16 in podman. The InfluxDB tag value comes
back out of a real database as `press 01,west`, space and comma intact, which
is the escaping bug that would otherwise fragment a series silently.

**Race detector: clean**, every package, on Linux with cgo.

**Not done yet.** Prometheus metrics beyond the counters the API already
exposes. The `template`, `delay`, `trigger` and `exec` nodes. HTTP In/Request,
WebSocket, TCP and UDP nodes. Subflow execution — subflows parse and round-trip
but instances do not run yet. Registering the chart in the dashboard's
`HelmRepoURLs`.

**Still not benchmarked against Node-RED.** Every number in this README is
Emberwire measured on my box. The comparison numbers do not exist because I have
not run the harness, and the RSS and throughput figures on the forums are
anecdotes with no controlled measurement behind them. They go in when I produce
them myself, on the same hardware, with the same flow, and not before.

## Compatibility

`flows.json` v1 loads and saves **byte for byte**. Load a file Node-RED wrote,
save it without editing anything, and you get the same bytes back — same key
order, same spacing, no rewritten escapes. Edit one property and the diff is one
line, not the whole node.

That is harder in Go than it sounds and I nearly did not bother. A JavaScript
object preserves key insertion order, so Node-RED gets this for free from
`JSON.parse` and `JSON.stringify`. A Go map has no order at all and
`encoding/json` deliberately sorts keys. Rather than force an order-preserving
map through every read path in the codebase, each entry's original bytes are kept
and re-emitted when the parsed form is unchanged, and re-encoded against the
original key order when it is not. `json.Compact` and `json.Indent` are byte-level
transforms, so they re-indent without reordering.

It matters for two reasons. Your flow file lives on a PVC and in git, and it
should not churn because a pod restarted. And when an operator reviews a deploy
diff before pushing it to a line, they should see what changed and nothing else.

A node type this build has never heard of survives a load-and-save with every
property intact, so it is safe to run a Node-RED-authored flow here and hand it
back.

**Node-RED community nodes do not work.** They are npm packages that need
Node.js. There is no version of this where they do, and I am not going to pretend
otherwise — that is the trade for the footprint and the sandbox.

Node-RED's `flows_cred.json` imports. Read-only: anything read that way is
re-encrypted under GCM on the next save.

Every node declares how it relates to its Node-RED counterpart, and there is a
test that fails the build if a node claims partial compatibility without saying
in writing what is missing. A node that is 90% compatible and quiet about it is
more dangerous than one that is obviously absent. The full matrix is in
[docs/compatibility.md](docs/compatibility.md).

## Layout

```
emberwire/
  cmd/emberwire/     the binary
  internal/
    engine/          messages, property expressions, the v1 flow graph
    node/            what a node type is: Descriptor, registry, contracts
    nodes/           the built-in palette
    runtime/         the scheduler, delivery, error and status routing
    store/           context, flow file, credentials
    js/              goja host
    wasmhost/        wazero host
    api/             admin REST and the editor websocket
    discover/        network discovery nodes
  web/               the editor
  charts/            the App Store chart
```

## Building

```bash
go test ./...
CGO_ENABLED=0 go build -trimpath -ldflags="-s -w" -o emberwire ./cmd/emberwire
```

Static, no cgo, runs on distroless. goja and wazero are both pure Go, which is
what keeps that true. CI fails the build if the binary exceeds 40MiB or turns out
to be dynamically linked.

## Licence

Apache 2.0. Emberwire is an independent implementation and contains no Node-RED
source — see [NOTICE](NOTICE) for the attribution and the list of deliberate
divergences.

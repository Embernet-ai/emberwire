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
| Function sandbox | `node:vm`, explicitly not a boundary | goja, plus a WASM ABI with real memory and CPU limits |
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

**Done and tested.** The engine core: message model, property expressions, the v1
flow parser, the scheduler with back-pressure, Catch/Status/Complete routing with
the group-distance rule, context stores, survivable storage, and the first wave of
the palette. Five packages, comprehensive tests, everything green.

**Not done yet.** The editor. The admin API and the binary — you cannot run this
yet, only import it as a library. The goja and WASM function hosts. Network and
parser nodes. The discovery node family. The Helm chart, the image, and the
benchmark harness.

**Not benchmarked against Node-RED yet.** The numbers above are Emberwire
measured on my box. The Node-RED comparison numbers do not exist yet because I
have not run the harness, and the RSS and throughput figures floating around the
forums are anecdotes with no controlled measurement behind them. They are not
going in here until I have produced them myself on the same hardware with the
same flow.

**The race detector has not run on my machine.** It needs cgo and there is no gcc
on this Windows box. CI runs it on ubuntu, which is what the binary actually ships
on.

## Compatibility

`flows.json` v1 loads and round-trips. A node type this build has never heard of
survives a load-and-save with every property intact, so it is safe to run a
Node-RED-authored flow here and hand it back.

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

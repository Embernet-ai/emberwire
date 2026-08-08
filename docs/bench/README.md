# Benchmarks

Every comparative number in this repository came out of `emberwire bench`. None
of them came off a forum.

That is the whole reason this directory exists. The throughput and memory
figures people quote for Node-RED are anecdotes — different hardware, different
flows, different Node versions, nothing controlled — and repeating one with our
name on it would be passing a rumour off as a measurement. So: one box, one
session, one load generator, one flow file, both runtimes.

## The flow

[`bench-flow.json`](bench-flow.json). Five nodes: an HTTP endpoint, three
property edits, a reply.

It is deployed to **both** runtimes **unchanged**. That is not a convenience, it
is the point — the file is Node-RED v1 and Emberwire reads it as-is, so there is
no translation step to argue about. Both answer `ok` with a 200 to the same
request.

Nothing in the flow is fast on purpose. It is the shape of a small real flow,
which is what the comparison is supposed to be about.

## Running it

Both runtimes as containers, so the comparison is between two things deployed
the way they are actually deployed:

```bash
podman build -t localhost/emberwire:bench .
podman pull docker.io/nodered/node-red:latest

mkdir -p /tmp/ewbench /tmp/nrbench
cp docs/bench/bench-flow.json /tmp/ewbench/flows.json
cp docs/bench/bench-flow.json /tmp/nrbench/flows.json

HASH=$(podman run --rm localhost/emberwire:bench hash-password -password benchbench123)
podman run -d --name ew-bench -p 18811:1880 -v /tmp/ewbench:/data:Z \
  -e EMBERWIRE_DATA_DIR=/data -e EMBERWIRE_ADMIN_USER=admin \
  -e EMBERWIRE_ADMIN_PASSWORD_HASH="$HASH" \
  -e EMBERWIRE_CREDENTIAL_SECRET=bench-secret -e EMBERWIRE_LOG_LEVEL=warn \
  localhost/emberwire:bench

podman run -d --name nr-bench -p 18812:1880 -v /tmp/nrbench:/data:Z \
  docker.io/nodered/node-red:latest
```

Then the same command against each, from the same shell:

```bash
emberwire bench -mode http -target http://127.0.0.1:18811 -path /bench \
  -duration 30s -warmup 5s -connections 8
emberwire bench -mode http -target http://127.0.0.1:18812 -path /bench \
  -duration 30s -warmup 5s -connections 8
```

Memory is read separately, because the harness's own RSS reader measures a
process it launched and these are containers:

```bash
podman stats --no-stream --format "{{.Name}} {{.MemUsage}}" ew-bench nr-bench
```

The load generator is closed-loop: each client sends the next request when the
last one came back. That measures what the far end can absorb, which is the
question, and it cannot produce the misleading coordinated-omission latency an
open-loop generator reports when the target falls behind.

The five-second warm-up is discarded. Node's JIT needs it. Ours does not, and it
costs nothing to be even-handed about it.

## Results

**Measured 2026-08-08.** Linux, 12 CPUs, both runtimes in rootless podman on the
same host, `nodered/node-red:latest` (Node-RED 4.x), Emberwire at `05d4235`.

| | Emberwire | Node-RED | |
|---|---|---|---|
| Image size | **25.1 MB** | 717 MB | 29× |
| Memory, idle | **4.8 MB** | 54.6 MB | 11× |
| Memory, under load | **12.3 MB** | 179.6 MB | 15× |
| Throughput | **3,460 req/s** | 1,290 req/s | 2.7× |
| Latency p50 | **1.03 ms** | 4.61 ms | 4.5× |
| Latency p95 | **9.19 ms** | 14.72 ms | 1.6× |
| Latency p99 | **16.72 ms** | 23.14 ms | 1.4× |
| Cold start | **2.1–2.3 s** | 4.6–5.5 s | ~2.3× |

Requests completed in the 30-second window: 103,800 against Emberwire, 38,700
against Node-RED.

### What these numbers are not

**Cold start includes container start.** Roughly two seconds of it is podman
bringing up the container, and that is common to both — so the runtime-only
difference is the delta, about 2.5–3 seconds, not the ratio.

**Throughput here includes the HTTP stack.** That is deliberate: it is the only
surface both runtimes present identically, and it is what somebody using the app
experiences. It is not a measurement of either scheduler in isolation. For that:

```
emberwire bench -mode engine -chain 5 -messages 200000
```

which pushes messages through a five-node chain with no I/O in the path and
reports messages per second, nanoseconds per message, and bytes allocated per
message. There is no equivalent for Node-RED, so **that number is never quoted
comparatively.**

**One flow shape, one payload size, one concurrency.** A flow doing real I/O —
an MQTT publish, a database write — would be dominated by the I/O and both
runtimes would converge. The claim being made is about the runtime's own
overhead, not about every workload.

**The p95 and p99 gaps are much smaller than the p50 gap.** Worth saying out
loud rather than quoting the p50 alone: under saturation both runtimes queue,
and queueing dominates the tail. The median is where the difference in
per-request cost actually shows.

## Re-running after a change

The numbers above are pinned to a commit. If the engine changes, re-run before
editing them, and re-run **both sides in the same session** — a Node-RED figure
carried over from an earlier run on a differently-loaded box is exactly the kind
of number this directory exists to avoid.

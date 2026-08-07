# Node compatibility

Generated from the node registry. Do not edit by hand — change the
`Compatibility` field on the node's descriptor and regenerate:

```
EMBERWIRE_UPDATE_DOCS=1 go test ./internal/nodes/
```

A node that is partially compatible and silent about how is worse than one
that is obviously absent: the flow appears to work and quietly does the wrong
thing. Every entry below has to say what is missing, and a test fails the
build if one does not.

## What the levels mean

| Level | Meaning |
|---|---|
| **full** | Behaves as the Node-RED node of the same type does. |
| **partial** | A subset. The notes say exactly which parts are missing. |
| **divergent** | Deliberately behaves differently. The notes say why. |
| **emberwire-only** | No Node-RED counterpart. |

## Not supported at all

**Node-RED community nodes.** They are npm packages that need Node.js.
There is no version of this where they work.

**JSONata expressions.** Any property typed `jsonata` is refused with an
error rather than ignored. Returning the expression text would make a flow
appear to work while routing on a literal string.

## Summary

33 node types registered.

| Level | Count |
|---|---|
| full | 8 |
| partial | 16 |
| divergent | 3 |
| emberwire-only | 6 |

## Common

| Type | Level | Notes |
|---|---|---|
| `catch` | full | — |
| `comment` | full | — |
| `complete` | full | Watches only the nodes explicitly selected in its scope, as Node-RED does. |
| `debug` | full | — |
| `inject` | partial | Interval and startup injection are supported. Cron-style scheduling ("at a specific time", "on these days") is not implemented in this build. Ignored properties: `crontab`. |
| `junction` | full | — |
| `link in` | full | — |
| `link out` | partial | Link Out in "send to" mode is supported. "Return to calling Link Call" requires the Link Call node, which is not implemented in this build. |
| `status` | full | — |

## Config

| Type | Level | Notes |
|---|---|---|
| `emberwire-influxdb` | emberwire-only | Emberwire's own InfluxDB connection, targeting the App Store's influxdb-app. |
| `emberwire-postgres` | emberwire-only | Emberwire's own PostgreSQL connection. Targets the App Store's postgresql-app and timescale-db-pod, which share a wire protocol. |
| `mqtt-broker` | partial | Connection, credentials, TLS, clean session, keepalive, birth and close messages are supported. Will messages and MQTT v5 properties are not implemented in this build. Ignored properties: `willTopic`, `willPayload`, `protocolVersion:5`. |

## Discover

| Type | Level | Notes |
|---|---|---|
| `netinfo` | emberwire-only | Emberwire's own node. Reports the interfaces the runtime can see, which in macvlan mode is how a flow learns its address on the OT VLAN. |
| `scan` | emberwire-only | Emberwire's own node. Sweeps a CIDR range for OT devices and identifies Modbus and EtherNet/IP endpoints. Bounded by the discovery allowlist in the runtime configuration, not by this dialog. |

## Function

| Type | Level | Notes |
|---|---|---|
| `change` | partial | set, change, delete and move are supported for msg, flow and global targets. JSONata-typed values are not evaluated in this build. |
| `delay` | divergent | All six modes are implemented — fixed, variable, random, rate limit, per-topic queue and timed release — along with msg.reset, msg.flush and the second output for dropped messages. Two deliberate differences: the queue is bounded, and past the limit a message is refused to a Catch node rather than held, because Node-RED's unbounded queue turns a source faster than the drain into an OOM-kill with no explanation; and messages still held when the flow stops are released rather than discarded. |
| `exec` | divergent | The three outputs, both buffered and streaming modes, the timeout and the appended message property all behave as Node-RED's do. Two things do not, and neither is negotiable. There is no shell: the command line is split on quoting rules only, and an unquoted shell metacharacter is refused rather than run, so one allowed command cannot become an arbitrary one. And the node is disabled until an operator names the commands a flow may run — Node-RED's exec node against a default configuration is CVE-2025-41656, unauthenticated remote code execution. Output is capped per stream; a command that exceeds it is killed and reported rather than being allowed to fill the heap. A command that forks children of its own may leave them behind when it is killed. |
| `function` | partial | Runs on goja, a JavaScript interpreter written in Go, rather than Node's vm module. The language is ES2023; the Node standard library is not present. require() and npm modules do not work and cannot be made to without embedding Node. There is always a CPU time limit, which Node-RED leaves optional and off. setTimeout and setInterval are not available — use a Delay or Trigger node, which the runtime can account for. Ignored properties: `libs`, `setTimeout`, `setInterval`, `require`. |
| `range` | full | — |
| `rbe` | partial | Block-unless-changed and deadband modes are supported. Narrowband modes are not implemented in this build. |
| `switch` | partial | All comparison operators are supported except jsonata_exp, which needs an expression engine this build does not ship. Ignored properties: `jsonata_exp`. |
| `template` | partial | Mustache templating is implemented against mustache.js's dialect, including its HTML escape set and standalone-line handling, so a template moved from Node-RED renders the same bytes. Partials ({{>name}}) and custom delimiters are refused rather than ignored, because there is nothing in a flow file that can supply either. |
| `trigger` | divergent | Both messages, extend-on-retrigger, wait-to-be-reset, msg.reset, the msg.delay override, per-topic grouping and the second output are implemented. The divergence is the same as the Delay node's: the number of simultaneously armed timers is bounded, and a message past the limit is refused to a Catch node rather than silently arming another. Timers with a deadline that are still armed when the flow stops fire immediately rather than being discarded; a timer waiting to be reset is dropped, because firing it would invent an event that never happened. |

## Network

| Type | Level | Notes |
|---|---|---|
| `mqtt in` | partial | Topic subscription with QoS and payload decoding are supported. Dynamic subscription via a control message is not implemented. |
| `mqtt out` | partial | Publishing with topic, QoS and retain from the node or the message is supported. MQTT v5 user properties are not implemented. |

## Parser

| Type | Level | Notes |
|---|---|---|
| `csv` | partial | Parsing to objects and rendering from objects are supported, with configurable separator and header handling. Multi-line quoted fields spanning separate messages are not reassembled. |
| `json` | partial | Conversion in both directions is supported. Schema validation against msg.schema is not implemented in this build. Ignored properties: `schema`. |

## Sequence

| Type | Level | Notes |
|---|---|---|
| `batch` | partial | Grouping by message count, with overlap, is supported. Time-interval and concatenate-sequences modes are not implemented. Ignored properties: `interval`, `concat`. |
| `join` | partial | Automatic mode rejoins sequences produced by Split, and manual mode joins by count. Timeout-based and reduce-sequence modes are not implemented. Ignored properties: `timeout`, `reduceRight`, `reduceExp`. |
| `sort` | partial | Sorts array payloads and message sequences by a property. JSONata key expressions are not supported. Ignored properties: `keyType:jsonata`. |
| `split` | partial | Splits arrays, objects, strings and buffers. Streaming mode, which carries a partial remainder between messages, is not implemented. Ignored properties: `stream`. |

## Storage

| Type | Level | Notes |
|---|---|---|
| `influxdb out` | emberwire-only | Emberwire's own node. The type name matches the community node-red-contrib-influxdb so an imported flow finds it, but the configuration is not identical — check the fields after importing. |
| `postgres` | emberwire-only | Emberwire's own node. Writes to and reads from PostgreSQL or TimescaleDB, with batch insert for message sequences. |


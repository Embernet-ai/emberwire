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

51 node types registered.

| Level | Count |
|---|---|
| full | 10 |
| partial | 26 |
| divergent | 9 |
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
| `websocket-client` | partial | Connects out and reconnects on its own when the connection drops, in payload mode or whole-message mode. Per-node TLS configuration is not implemented; the system trust store is used. Ignored properties: `tls`. |
| `websocket-listener` | partial | Serves a websocket path, in payload mode or whole-message mode. The path shares the flow route table with the HTTP In nodes, so it cannot shadow the editor or the admin API and cannot collide with another node's path. A client that stops reading is disconnected rather than queued without limit, which Node-RED does not do. |

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
| `http in` | divergent | Serves a path, with Express-style :params and a trailing *, and builds the same msg.req / msg.res / msg.payload shape Node-RED does, including JSON, form-encoded and raw bodies. Three deliberate differences. A request that no HTTP Response node answers is closed with 504 after a timeout instead of being held open forever, because Node-RED's version leaks a connection per request until the process runs out of sockets and stops answering with nothing in the log. Two nodes claiming the same method and path is refused at deploy time rather than one of them silently never firing. And a path that would shadow the editor or the admin API is refused for the same reason. File uploads are not parsed into msg.files; a multipart body arrives as raw bytes. Ignored properties: `upload`, `swaggerDoc`. |
| `http request` | partial | Method, URL, headers, basic authentication, redirects and the three return types, with msg.url, msg.method and msg.headers overriding the node. The response body is size-capped and the call is bounded by a timeout, neither of which Node-RED does. Cookie jars, proxy settings, per-node TLS configuration and connection persistence are not implemented in this build. There is no egress allowlist: this node can reach anything the pod can, exactly as Node-RED's can, and the place to bound that is a NetworkPolicy rather than an edit dialog nobody outside the cluster can trust. Ignored properties: `proxy`, `tls`, `persist`, `cookies`. |
| `http response` | partial | Status code and headers from the node or from msg.statusCode and msg.headers, with the payload as the body. Cookies set through msg.cookies are not implemented; set a Set-Cookie header instead. Ignored properties: `cookies`. |
| `mqtt in` | partial | Topic subscription with QoS and payload decoding are supported. Dynamic subscription via a control message is not implemented. |
| `mqtt out` | partial | Publishing with topic, QoS and retain from the node or the message is supported. MQTT v5 user properties are not implemented. |
| `tcp in` | divergent | Listens or connects out, in stream mode with a delimiter or single mode collecting until the peer closes, with buffer, string or base64 payloads. Every message carries msg._session so a TCP Out node can reply on the same connection. Three bounds Node-RED does not have: the number of accepted connections, the size of one delimited message, and the total of a single-mode read. A peer that opens connections and never closes them, or sends without ever sending the delimiter, grows the heap until the pod dies otherwise. TLS is not implemented. Ignored properties: `tls`. |
| `tcp out` | partial | Connects to a host and sends, or replies on the connection a TCP In node accepted, found through msg._session. Listening for inbound connections purely to write to them, which Node-RED's third mode does, is not implemented — use a TCP In node for the listening half and this node in reply mode. TLS is not implemented. Ignored properties: `tls`. |
| `tcp request` | partial | Connects, sends the payload and waits for the reply, in any of Node-RED's four wait modes: a fixed time, a delimiter, a byte count, or until the peer closes. msg.host and msg.port override the node. Every request opens its own connection — Node-RED's connection-reuse mode is not implemented, and reusing one would change the semantics of the wait modes, which all end at a connection boundary. TLS is not implemented. Ignored properties: `tls`. |
| `udp in` | partial | Receives datagrams, optionally joining a multicast group, with buffer, string or base64 payloads and the sender's address on msg.ip and msg.port. IPv6 and per-node interface selection are not implemented in this build. Ignored properties: `ipv6`. |
| `udp out` | partial | Sends datagrams to a host, a broadcast address or a multicast group, with msg.ip and msg.port overriding the node. IPv6 is not implemented in this build. Ignored properties: `ipv6`. |
| `websocket in` | full | Emits a message per frame, carrying msg._session so a WebSocket Out node can reply to the connection it came from. |
| `websocket out` | divergent | Replies to the connection named by msg._session, or broadcasts to every open connection when there is none, as Node-RED does. The divergence is what happens when a connection cannot keep up: the frame is refused to a Catch node and the connection is closed, rather than queued without limit. Blocking instead would push back-pressure from a slow browser into the flow's scheduler, which is a worse failure than losing a connection that had already stopped reading. |

## Parser

| Type | Level | Notes |
|---|---|---|
| `csv` | partial | Parsing to objects and rendering from objects are supported, with configurable separator and header handling. Multi-line quoted fields spanning separate messages are not reassembled. |
| `html` | partial | Extracts elements by CSS selector, returning inner HTML, text or attributes, as one message per match or one message holding an array. The selector engine covers type, id, class, attribute, descendant, child and comma groups. Pseudo-classes, pseudo-elements, sibling combinators and the ~= and \|= attribute operators are refused at deploy time rather than ignored, because a selector that quietly drops its :nth-child matches the wrong elements and keeps working. Returned HTML is re-rendered from the parse tree, so it is normalised markup rather than the original bytes. |
| `json` | partial | Conversion in both directions is supported. Schema validation against msg.schema is not implemented in this build. Ignored properties: `schema`. |
| `xml` | partial | Both directions, using xml2js's object convention: attributes under the key named by "attr" (default $), element text under "chr" (default _), and every child element as an array, which is xml2js's own explicitArray default. Set ew_explicitArray to false to collapse single children instead. The per-message msg.options that Node-RED passes through to xml2js is not honoured — the other xml2js options change the shape of the output, and silently ignoring one would produce an object the flow does not expect while looking like it worked. Namespaces are kept as part of the element name rather than being resolved. Ignored properties: `options`. |
| `yaml` | full | Conversion in both directions, toggling on the value's type when no action is set, as Node-RED's does. |

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
| `file` | divergent | Append, overwrite and delete, with the filename from a literal, a message property, context or the environment, and utf8, base64, hex or raw encodings. The divergence is the path scope: the file nodes may only reach the data directory and whatever else the operator listed, resolved through symlinks so a link planted on the PVC cannot point out of it. Node-RED's file nodes take any path, which makes editing a flow equivalent to reading any file the process can. Writes are fsynced by default, which Node-RED's are not. |
| `file in` | divergent | Whole-file, per-line and chunked reads, with utf8, base64, hex or raw output. Same path scope as the File node, and for the same reason. A read is also size-capped: Node-RED reads a whole file into memory with no limit, so pointing the node at the wrong path is an OOM-kill rather than an error. Past the cap the node says which limit was hit and that per-line or chunked mode would work. |
| `influxdb out` | emberwire-only | Emberwire's own node. The type name matches the community node-red-contrib-influxdb so an imported flow finds it, but the configuration is not identical — check the fields after importing. |
| `postgres` | emberwire-only | Emberwire's own node. Writes to and reads from PostgreSQL or TimescaleDB, with batch insert for message sequences. |
| `watch` | divergent | Reports files and directories appearing, changing and being removed, with the same message shape Node-RED produces. It polls rather than using the kernel's notification interface, so a change is seen within the poll interval rather than immediately, and two changes inside one interval are reported once. That is a deliberate trade: fsnotify means per-platform code and a filename suffix that is a build constraint, which has already cost this codebase a day. Same path scope as the other file nodes, and the number of watched entries is capped so a recursive watch on a large tree cannot stall the runtime. |


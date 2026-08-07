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

17 node types registered.

| Level | Count |
|---|---|
| full | 8 |
| partial | 9 |
| divergent | 0 |
| emberwire-only | 0 |

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

## Function

| Type | Level | Notes |
|---|---|---|
| `change` | partial | set, change, delete and move are supported for msg, flow and global targets. JSONata-typed values are not evaluated in this build. |
| `range` | full | — |
| `rbe` | partial | Block-unless-changed and deadband modes are supported. Narrowband modes are not implemented in this build. |
| `switch` | partial | All comparison operators are supported except jsonata_exp, which needs an expression engine this build does not ship. Ignored properties: `jsonata_exp`. |

## Sequence

| Type | Level | Notes |
|---|---|---|
| `batch` | partial | Grouping by message count, with overlap, is supported. Time-interval and concatenate-sequences modes are not implemented. Ignored properties: `interval`, `concat`. |
| `join` | partial | Automatic mode rejoins sequences produced by Split, and manual mode joins by count. Timeout-based and reduce-sequence modes are not implemented. Ignored properties: `timeout`, `reduceRight`, `reduceExp`. |
| `sort` | partial | Sorts array payloads and message sequences by a property. JSONata key expressions are not supported. Ignored properties: `keyType:jsonata`. |
| `split` | partial | Splits arrays, objects, strings and buffers. Streaming mode, which carries a partial remainder between messages, is not implemented. Ignored properties: `stream`. |


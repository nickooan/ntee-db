# ntee-db-client (Node.js)

Pure-JavaScript TCP client for **nteedb-server** — the network daemon in front
of [nteedb](https://github.com/nickooan/ntee-db), a log-structured KV store
with secondary indexes and atomic counters. **Zero runtime dependencies**:
just `node:net` and the standard library.

- Covers **every server command** — KV reads/writes, indexed writes, all
  secondary-index queries, atomic counters (`incr`/`decr`/`topup`/`take`),
  **per-key TTL** (lazy expiry; optional `ttlMs` on every write), range
  deletes, stats, and the admin commands.
- **Pipelines natively**: fire commands without awaiting; responses resolve in
  order over one socket.
- Always uses the **length-prefixed wire form** for writes, so values can be
  binary, empty, multi-line, or larger than the server's 1 MiB line limit —
  none of the inline-`put` pitfalls.
- Automatic value decoding: JSON values arrive parsed, anything else as a
  `Buffer`.
- Auth support for both server modes (shared password and user file), with
  role-aware errors.
- **TLS** — connect to the server's TLS listener with one option (`tls`),
  including custom CAs for self-signed deployments.
- Typed (`src/index.d.ts`), ESM, Node ≥ 20.

## Install

```sh
npm install ntee-db-client
```

You also need a running server — see
[`nteedb-server`](../nteedb-server/README.md):

```sh
nteedb-server -schema schema.json
# or from the repo:
go run ./nteedb-server -schema nteedb-server/schema.example.json -dir /tmp/nteedb
```

## Quick start

```js
import { NteeClient } from "ntee-db-client"

const db = await NteeClient.connect({ host: "127.0.0.1", port: 6666 })
console.log(db.info) // { server: "nteedb", version, auth, indexes: [...] }

// KV — objects round-trip as parsed JSON.
await db.put("user:1", { name: "Ada" })
await db.get("user:1") // { name: "Ada" }
await db.get("missing") // null

// Indexed write + secondary-index queries.
await db.put("call:1", { kind: "request" }, { traceId: "T1", status: 200 })
await db.secIndex("traceId", "T1") // ["call:1"]
await db.secIndexRange("status", 200, 299) // ["call:1"]

// Atomic counters — including the bounded quota/stock operators.
await db.incr("hits") // 1
await db.topup("quota", 10, 100) // 0 = fully applied (overflow count)
await db.take("quota", 3, 0) // true = applied (all-or-nothing)

// TTL: optional ttlMs on any write. On counters it applies on create only,
// which makes this line a complete fixed-window rate limiter:
await db.incr("rl:user1", 1, 60_000) // arms a 60s window on first request
await db.put("session:9", { user: 1 }, undefined, 30 * 60_000) // ephemeral value

await db.close()
```

## Connection options

```js
await NteeClient.connect({
  host: "127.0.0.1", // default
  port: 6666, // default; 6667 when `tls` is set (the server's -addr / -tls-addr defaults)
  tls: { ca }, // connect to the TLS listener (see below); omit for plain TCP
  auth: "s3cret", // password mode … or:
  auth: { user: "bob", password: "hunter2" }, // auth-file mode
  connectTimeout: 5000, // ms; guards the dial + handshake only
})
```

`connect()` authenticates (when `auth` is given) and runs the `hello`
handshake; the result is exposed as `client.info`. Against an
auth-requiring server without credentials, `connect()` still succeeds —
`client.info.auth` tells you the mode, and data commands fail with
`auth required` until you call `client.auth(...)`.

### TLS

The server runs a TLS listener when started with `-tls-cert`/`-tls-key`
(default `127.0.0.1:6667`). Pass the `tls` option to use it:

```js
// Certificate signed by a public CA — verify against the system roots:
await NteeClient.connect({ host: "db.example.com", tls: true })

// Self-signed / private CA — pass the CA certificate:
import { readFile } from "node:fs/promises"
const ca = await readFile("cert.pem")
await NteeClient.connect({ host: "10.0.0.5", tls: { ca }, auth: "s3cret" })

// Skip verification (testing only — vulnerable to interception):
await NteeClient.connect({ tls: { rejectUnauthorized: false } })
```

An object `tls` value is passed through to
[`tls.connect`](https://nodejs.org/api/tls.html), so `servername`, client
certificates, and every other node:tls option work as-is. With `tls` set,
the default port becomes 6667.

## Values

Stored values are bytes; the server encodes them in responses by content:

- Bytes that are **valid JSON** arrive parsed — store an object, get an
  object back. Note the corollary: the _string_ `"123"` is valid JSON, so it
  comes back as the number `123`.
- Anything else — plain text, binary — arrives as a **`Buffer`**.
- **Counters** are stored in a fixed-width text form that is not valid JSON,
  so `get` on a counter returns a `Buffer`. Read counters with
  `incr(key, 0)`, which returns the number.
- `put` accepts `string | Buffer` (stored verbatim) or any JSON-serializable
  value (stringified). `undefined` throws.

## API

Every method maps 1:1 to a server command. Method names mirror the embedded
[`ntee-db`](../nteedb-js/README.md) binding where the operation matches.

| Method                                                 | Command                 | Resolves to                                                            |
| ------------------------------------------------------ | ----------------------- | ---------------------------------------------------------------------- |
| `NteeClient.connect(opts)`                             | `auth` + `hello`        | connected client (`.info` set)                                         |
| `hello()`                                              | `hello`                 | `{server, version, auth, indexes?}`                                    |
| `auth(pw \| {user, password})`                         | `auth`                  | `true` (refreshes `.info`)                                             |
| `ping()`                                               | `ping`                  | `"pong"`                                                               |
| `quit()` / `close()`                                   | `quit`                  | graceful disconnect, idempotent                                        |
| `get(key)`                                             | `get`                   | value or `null`                                                        |
| `getMany(keys)`                                        | `getm`                  | values aligned to `keys` (`null` for misses)                           |
| `has(key)`                                             | `has`                   | `boolean`                                                              |
| `prefixScan(prefix?)`                                  | `scan`                  | sorted keys                                                            |
| `secIndex(name, val, limit?)`                          | `ix`                    | keys (`0` all asc, `N` first N, `-N` last N desc)                      |
| `secIndexHas(name, val)`                               | `ixh`                   | `boolean`                                                              |
| `secIndexPrefix(name, prefix, limit?)`                 | `ixp`                   | keys (limit applies per distinct value)                                |
| `secIndexRange(name, lo, hi)`                          | `ixr`                   | keys (inclusive bounds)                                                |
| `secIndexRecords(name, val, limit?)`                   | `ixrec`                 | `[{key, value}]` decoded                                               |
| `put(key, value)`                                      | `put` (length-prefixed) | `true`                                                                 |
| `put(key, value, ix)`                                  | `putx`                  | `true` (value must be a JSON object)                                   |
| `put(key, value, ix?, ttlMs)`                          | `putex` / `putx` + ttl  | `true` — write with a time-to-live                                     |
| `delete(key)`                                          | `del`                   | `true` (even if absent)                                                |
| `incr(key, delta?, ttlMs?)` / `decr(...)`              | `incr`/`decr`           | new counter value (ttl applies on create only)                         |
| `topup(key, amount, max, ttlMs?)`                      | `topup`                 | overflow that didn't fit (`0` = fully applied)                         |
| `take(key, amount, left, ttlMs?)`                      | `take`                  | `true` iff applied                                                     |
| `removeByPkLess(cutoff)` / `removeByPkGreater(cutoff)` | `rml`/`rmg`             | deleted count                                                          |
| `stats()`                                              | `stats`                 | `{records, mainBytes, liveBytes, blobBytes, connections, …}`           |
| `secIndexDropped()` / `secIndexProspective()`          | `dropped`/`prospective` | index names                                                            |
| `compact()` / `reindex()` / `relieve()`                | same                    | `true` (admin role; may take seconds)                                  |
| `command(line)`                                        | _(any)_                 | raw envelope `{ok, found?, result?, err?}`, never throws on `ok:false` |

## Errors

- **`NteeServerError`** — the server answered `{"ok":false}`. Carries
  `.command` (the failing command word) and the server's error text
  (`auth required`, `nteedb: unknown index "x"`, `permission denied: compact
requires admin`, …). The connection remains usable.
- **`NteeConnectionError`** — the socket failed, the server closed, the
  response stream desynced, or the client was used after `close()`. All
  in-flight commands reject with it. **There is no auto-reconnect** — create
  a new client with `connect()`.
- **`TypeError`** — thrown synchronously for client-side misuse: whitespace
  in a key/index value, non-integer counter arguments, `undefined` values.

One error is both: an oversized command line or value (server limits: 1 MiB
per line, 32 MiB per value) rejects the offending command with the server's
error _and then the server closes the connection_ — treat it as fatal.

## TTL (lazy expiry)

Any write can carry a `ttlMs`. Expiry lives in the server's primary-key
index and is enforced **lazily**: once the TTL passes, the key reads as
missing everywhere (`get`/`has`/`getMany`/`prefixScan`) and a background
reaper deletes it durably; leftovers are dropped by `compact`/`reindex`.

The TTL rules, per operation:

- **No write has a TTL unless you pass one** — keys are immortal by default.
- **`put` replaces the record wholesale, so its TTL state wins**: with
  `ttlMs` it arms/replaces the expiry; without, it **clears** an existing
  one (Redis SET style).
- **Counter ops (`incr`/`decr`/`topup`/`take`) never touch a live counter's
  TTL — in either direction.** Omitting `ttlMs` does NOT clear it (unlike
  `put`), and passing `ttlMs` on a live counter is ignored (create-only —
  a window's deadline never slides because of traffic).
- **An expired counter is recreated fresh by the next op — and then the
  `ttlMs` arg decides.** With it, the new window/bucket is armed; without
  it, the recreated counter is **immortal**. So for expiring windows and
  buckets, the correct idiom is to pass `ttlMs` on **every** call
  (`incr("rl:u1", 1, 60000)`, `topup("bucket", n, cap, ttl)`): it's a no-op
  on live keys and does the right thing on lapsed ones.

Caveat: secondary indexes are TTL-unaware, so an expired key can linger in
`secIndex`/`secIndexHas`/... key results until its cleanup runs —
`secIndexRecords` and `getMany` already drop it. There is no remaining-TTL
query; re-arm by rewriting with `put(..., ttlMs)`.

## Notes / limitations

- **No whitespace in keys, index names, string index values, or
  credentials** — the wire protocol splits on whitespace with no quoting.
  The client throws a `TypeError` before sending. Values are exempt (they
  travel length-prefixed).
- **int64 precision**: counters, byte sizes, and delete counts are int64 on
  the server; `JSON.parse` loses precision above 2^53.
- **Idle timeout**: the server silently closes connections idle for 5
  minutes (its `-idle` flag). Long-lived clients should `ping()`
  periodically or reconnect on `NteeConnectionError`.
- One documented decode ambiguity: a value stored as the literal JSON object
  `{"bin": true, "base64": "…"}` (exactly those two keys) is
  indistinguishable from the server's binary wrapper and decodes to a
  `Buffer`.
- Use TLS (or loopback / a trusted network) in production — the plain
  listener sends credentials and data in cleartext. The server refuses
  non-loopback binds without auth unless `-insecure`, and that rule covers
  its TLS listener too (TLS encrypts but does not authorize).

## Testing

```sh
npm test
```

The tests spawn a **real server**: they build `../nteedb-server` from this
repository once per run, so a **Go toolchain** must be on `PATH`. Each
fixture gets a fresh store directory and an ephemeral port, so suites run in
parallel. `example.mjs` is a runnable tour against a manually started server
(`node example.mjs`, honoring `NTEEDB_HOST`/`NTEEDB_PORT`/`NTEEDB_AUTH`).

## License

Apache-2.0

# nteedb-server

A standalone TCP server for **ntee-db** — run the store as a small daemon (in
the spirit of redis/memcached) and talk to it from any language. The protocol
is a memcached-style **text protocol**; every response is exactly **one line
of JSON**. Pure Go, no dependencies beyond the core.

```sh
go run github.com/nickooan/ntee-db/nteedb-server@latest -schema schema.json
# or, in this repo:
go run ./nteedb-server -schema nteedb-server/schema.example.json
```

Concurrency: one goroutine per connection; the core's RWMutex lets reads
(`get`/`scan`/`ix*`…) from many connections run **in parallel**, while writes
serialize. Pipelining is supported — send several commands, read the responses
in order.

## Starting

```sh
nteedb-server -schema schema.json                # 127.0.0.1:6740
nteedb-server -schema schema.json -addr 0.0.0.0:6740 -auth s3cret
nteedb-server -schema schema.json -auth-file users.txt
```

| Flag | Meaning |
| --- | --- |
| `-schema <file>` | store definition (required, below) |
| `-addr host:port` | listen address (default `127.0.0.1:6740`) |
| `-dir <path>` | store directory, overrides the schema's `dir` |
| `-auth <password>` | shared password (or `NTEEDB_AUTH` env); grants admin |
| `-auth-file <file>` | `user:password[:role]` lines; role `admin` or `user` (default) |
| `-insecure` | allow a non-loopback bind without auth (trusted network) |
| `-idle <dur>` | per-connection idle timeout (default `5m`, `0` disables) |

The store allows a **single writer process** (kernel `flock`): if another
process has the directory open, the server exits with a clear error at start.

### schema.json

Same shape as the JS binding's open options:

```json
{
  "dir": "/var/lib/nteedb",
  "blobThreshold": 65536,
  "syncEveryWrite": false,
  "hintEveryN": 1000,
  "autoCompact": false,
  "indexes": [
    { "name": "traceId", "kind": "string" },
    { "name": "kind", "kind": "string", "jsonPath": "kind" },
    { "name": "status", "kind": "number", "maxPerValue": 100 }
  ]
}
```

`jsonPath` (dotted path into the record) derives the index value on every
write; indexes without it take explicit values via `putx`.

### Auto-compaction

The log is append-only: overwrites, deletes, and range-deletes leave dead
lines behind until a compaction rewrites the file. Embedded users compact on
close; a daemon never closes — so with `"autoCompact": true` the server does
it itself. **Off by default** (an omitted field is `false`): without it,
compaction happens only via the manual admin `compact` command — the same
stance as the JS binding, which exposes `compact()` and leaves the policy to
the caller. When enabled, every 30 s the server computes the **dead-space ratio**
(`1 − liveBytes/mainBytes`, both visible in `stats`) and runs a compaction
when the ratio reaches **50%** and the log is at least **1 MiB** (compacting a
tiny log is pointless churn).

While a compaction runs, **reads keep working** — the core rebuilds the log
holding only its compaction gate and takes the exclusive lock just for the final
atomic file swap. Writes issued during the rebuild simply wait and apply once
it finishes. Each run is logged (`auto-compact: main 2114970 → 361226 bytes in
87ms`) and counted in `stats.autoCompacts`. Manual `compact` and `relieve`
(admin) are still available regardless of the flag.

The controller also watches the blob files, as an **independent trigger**: the
core exposes the measurement (`BlobUsage`: total/live/orphaned bytes) and the
mechanism (`BlobsRelieve`: unconditional rewrite that drops orphans); the
policy lives here in the server — when total blob bytes reach **64 MiB** and
≥ **50%** are orphaned (or a crashed relieve left a stray generation file),
the server runs `BlobsRelieve`. The floor is deliberately high: a blob rewrite
copies live blob contents, real I/O. Blob rewrites are counted in
`stats.blobCompacts` and log as `…, blobs 91226112 → 41943040 bytes`.

**Tuning.** `autoCompact` also accepts an options object (any field may be
omitted — its default applies; an object implies `"enabled": true` unless set
to false):

```json
"autoCompact": {
  "enabled": true,
  "intervalSeconds": 30,
  "mainRatio": 0.5,
  "mainMinBytes": 1048576,
  "blobsRelieve": true,
  "blobRatio": 0.5,
  "blobMinRelieveDataSize": 67108864
}
```

| Field | Default | Meaning |
| --- | --- | --- |
| `intervalSeconds` | 30 | policy check cadence |
| `mainRatio` | 0.5 | main-log dead share that triggers `Compact` |
| `mainMinBytes` | 1 MiB | main-log size floor |
| `blobsRelieve` | true | `false` disables the automatic blob trigger entirely (manual `relieve` still works) |
| `blobRatio` | 0.5 | orphaned blob share that triggers `BlobsRelieve` |
| `blobMinRelieveDataSize` | 64 MiB | blob size floor |

**How long is that write-pause?** Compaction is O(live bytes) — roughly
~4 µs per live record (dead records are skipped, blob contents are never
copied). Measured on an Apple M2 Pro with ~120-byte JSON records, one
secondary index, ~50% dead space, warm page cache:

| Live records | Log (before → after) | Compact time |
| ------------ | -------------------- | ------------ |
| 10,000       | ~2 MB → 2 MB         | 68 ms        |
| 100,000      | ~23 MB → 15 MB       | 424 ms       |
| 300,000      | ~70 MB → 45 MB       | 1.25 s       |

Reads are unaffected throughout; only writes arriving mid-compaction wait, at
most for the durations above. (Cold-cache compaction on a slow disk leans
toward the disk's sequential-read speed instead — still sub-second at these
sizes on any SSD.)

### Auth

Modeled on redis/memcached: credentials are configured at startup, a client
authenticates **once per connection** with the `auth` command.

- **No auth (default)** — like memcached, for local/trusted use only. Protected
  mode (borrowed from redis) refuses to bind non-loopback addresses unless
  `-insecure` is passed.
- **Password** (`-auth` / `NTEEDB_AUTH`) — redis `requirepass` style:
  `auth <password>`. Grants **admin**.
- **Users file** (`-auth-file`) — memcached `--auth-file` style:

  ```
  # user:password[:role]
  app1:secretA            # role "user": data commands + stats
  ops:secretB:admin       # role "admin": everything (compact, reindex)
  ```

Before auth, only `auth`, `ping`, `hello`, and `quit` are accepted.

## Protocol

Requests are single lines (`\r\n` or `\n`), tokens separated by spaces.
Responses are one JSON line: `{"ok":true,"result":…}` on success,
`{"ok":false,"err":"…"}` on failure. Try it with `nc`:

```
$ nc 127.0.0.1 6740
ping
{"ok":true,"result":"pong"}
put call:1 {"kind":"request","ms":42}
{"ok":true,"result":true}
get call:1
{"ok":true,"found":true,"result":{"kind":"request","ms":42}}
ix kind request
{"ok":true,"result":["call:1"]}
```

### Reads (parallel)

| Command | Core call | Result |
| --- | --- | --- |
| `get <pk>` | Get | `found` + value (`result:null` on miss) |
| `getm <pk> <pk> …` | GetMany | `[{key,found,value},…]` |
| `has <pk>` | Has | `true/false` |
| `scan [<prefix>]` | PrefixScan | sorted keys |
| `ix <name> <value> [±N]` | ByIndex | primary keys (`+N` first, `-N` last) |
| `ixh <name> <value>` | ByIndexHas | `true/false` |
| `ixp <name> <prefix> [±N]` | ByIndexPrefix | keys (limit grouped per value) |
| `ixr <name> <lo> <hi>` | ByIndexRange | keys in range |
| `ixrec <name> <value> [±N]` | ByIndex+GetMany | `[{key,value},…]` |

Values of `number`-kind indexes are parsed from the token (`ix status 200`).

### Writes (serialized)

| Command | Core call | Result |
| --- | --- | --- |
| `put <pk> <nbytes>` + value block | Put | `true` — length-prefixed: exactly `nbytes` raw bytes follow, then a newline |
| `put <pk> <inline value…rest of line>` | Put | `true` — sugar for single-line values |
| `putx <pk> <ixbytes> <nbytes>` + 2 blocks | PutIndexed | `true` — index-values JSON block, then value block |
| `del <pk>` | Delete | `true` |
| `rml <cutoff>` | RemoveByPkLess | count of deleted keys (`< cutoff`) |
| `rmg <cutoff>` | RemoveByPkGreater | count of deleted keys (`> cutoff`) |

A full `putx` frame on the wire:

```
putx call:1 29 18\r\n
{"traceId":"T1","status":200}\r\n
{"kind":"request"}\r\n
```

The **length-prefixed** `put` form carries any bytes (raw binary, multi-line
pretty JSON — the server reads exactly `nbytes`, no escaping/base64 needed).
The **inline** form is sugar for single-line values; note `put k 42` parses as
the length-prefixed form (42 bytes follow) — store bare numbers with it.

### Values are JSON

Stored values that are valid JSON come back **embedded verbatim** in the
response. Anything else (binary, non-JSON text) comes back wrapped:
`{"bin":true,"base64":"…"}` — so every response line stays parseable JSON.

### Session / admin

| Command | Notes |
| --- | --- |
| `auth <password>` / `auth <user> <password>` | per connection, first |
| `hello` | server name, version, auth mode, declared indexes |
| `ping`, `quit` | |
| `stats` | core stats + `liveBytes`, `connections`, `totalConns`, `commands`, `autoCompacts`, `blobCompacts` |
| `dropped`, `prospective` | index lifecycle introspection |
| `compact`, `reindex` | **admin role** — they hold the write lock while running |
| `relieve` | **admin role** — unconditional blob reclamation (+ main-log compact); the auto-path thresholds don't apply to this explicit action |

Ordinary command errors never close the connection. Protocol-level failures
(oversized line/value, malformed data block) do, because the stream position
is no longer trustworthy.

Limits: command line ≤ 1 MiB (use length-prefixed `put` beyond that), value
≤ 32 MiB. Graceful shutdown on SIGINT/SIGTERM closes the store cleanly (the
index hint is written, so the next boot is fast).

## Future work

Raw-framed `getr` for binary-heavy workloads, TLS, per-user ACLs, unix-domain
socket listener, metrics endpoint, and a Go client package. (A batch `mput`
was considered and dropped: pipelining already amortizes round-trips, and the
one-fsync-per-batch benefit only matters under `syncEveryWrite`.)

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
  "indexes": [
    { "name": "traceId", "kind": "string" },
    { "name": "kind", "kind": "string", "jsonPath": "kind" },
    { "name": "status", "kind": "number", "maxPerValue": 100 }
  ]
}
```

`jsonPath` (dotted path into the record) derives the index value on every
write; indexes without it take explicit values via `putx`.

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

```
put <pk> <nbytes>\r\n
<value: exactly nbytes raw bytes>\r\n

put <pk> <inline value…to end of line>

putx <pk> <ixbytes> <nbytes>\r\n
<index values JSON, e.g. {"traceId":"T1","status":200}>\r\n
<value bytes>\r\n

del <pk>
rml <cutoff>        delete every key < cutoff   → count
rmg <cutoff>        delete every key > cutoff   → count
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
| `stats` | core stats + `connections`, `totalConns`, `commands` |
| `dropped`, `prospective` | index lifecycle introspection |
| `compact`, `reindex` | **admin role** — they hold the write lock while running |

Ordinary command errors never close the connection. Protocol-level failures
(oversized line/value, malformed data block) do, because the stream position
is no longer trustworthy.

Limits: command line ≤ 1 MiB (use length-prefixed `put` beyond that), value
≤ 32 MiB. Graceful shutdown on SIGINT/SIGTERM closes the store cleanly (the
index hint is written, so the next boot is fast).

## Future work

`mput` (batch put in one round-trip), raw-framed `getr` for binary-heavy
workloads, TLS, per-user ACLs, unix-domain socket listener, metrics endpoint,
and a Go client package.

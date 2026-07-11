# ntee-db

A small, embedded, log-structured **JSON key-value store** written in pure Go,
with prefix search and secondary indexes — usable from **Go** and **Node.js**.

An append-only JSONL file is the source of truth; an in-memory B-tree index
serves fast lookups. No server, no separate process — the store runs inside
your app, in the same spirit as `lmdb` or SQLite in embedded mode.

**Highlights**

- **Log-structured & crash-safe** — the data log _is_ the WAL; torn writes are
  detected and truncated on boot, and the index is always rebuildable.
- **Secondary indexes** — string/number, multi-value, with exact / prefix /
  numeric-range queries, `±N` first/last limits, and automatic per-value
  capping (`MaxPerValue`: keep only the newest N records per value).
- **Prefix scans** on the primary key; **range delete** by primary key for
  time-based pruning.
- **Fast boot** via a persisted index snapshot (hint) + log-tail replay.
- **Hybrid memory** — only keys + offsets are resident; values and large blobs
  stay on disk and are read on demand.
- **Single-writer safety** via kernel `flock` (releases on any process exit).
  Unix (macOS/Linux) only.
- Pure Go; the only dependency is
  [`tidwall/btree`](https://github.com/tidwall/btree) (MIT).

## Packages

| Directory                    | What it is                                                                                                                            |
| ---------------------------- | ------------------------------------------------------------------------------------------------------------------------------------- |
| [`nteedb-core/`](nteedb-core/) | The store itself — a Go library (`package nteedb`). Full design & API docs in its [README](nteedb-core/README.md).                    |
| [`nteedb-js/`](nteedb-js/)     | [`ntee-db`](nteedb-js/README.md), the Node.js binding — the core compiled as a c-shared library (source in `nteedb-js/capi/`), loaded via FFI, shipped with prebuilt binaries. |
| [`nteedb-server/`](nteedb-server/) | A standalone TCP server (redis/memcached-style daemon): text protocol, single-line JSON responses, parallel reads, optional auth. Protocol docs in its [README](nteedb-server/README.md). |

(The JS binding stays server-free — it embeds the core directly.)

## Running the server

```sh
go run ./nteedb-server -schema nteedb-server/schema.example.json
```

```
$ nc 127.0.0.1 6740
put call:1 {"kind":"request"}
{"ok":true,"result":true}
ix kind request
{"ok":true,"result":["call:1"]}
```

## Using from Go

```sh
go get github.com/nickooan/ntee-db
```

```go
import nteedb "github.com/nickooan/ntee-db/nteedb-core"

db, err := nteedb.Open(nteedb.Options{Dir: "/path/to/store"})
if err != nil { /* ... */ }
defer db.Close()

db.Put("input:GetOrders", []byte(`{"q": "orders"}`))
v, ok, err := db.Get("input:GetOrders")
keys, err := db.PrefixScan("input:Get")
```

See [`nteedb-core/README.md`](nteedb-core/README.md) for the full story:
design, key ordering, secondary indexes, `MaxPerValue` capping, schema
migration, and all options.

## Using from Node.js

```sh
npm install ntee-db
```

```js
import { NteeDB } from "ntee-db"

const db = NteeDB.open("/path/to/store", {
  indexes: [{ name: "traceId", kind: "string" }],
})

db.put("call:1", { kind: "request" }, { traceId: "T1" })
const rec = await db.get("call:1") // parsed JSON
await db.secIndex("traceId", "T1") // ['call:1', ...]
db.close()
```

Prebuilt binaries ship for **darwin-arm64, linux-amd64, linux-arm64** — no Go
toolchain needed at install time. See
[`nteedb-js/README.md`](nteedb-js/README.md) for the API, benchmarks vs
`lmdb`/`better-sqlite3`, and notes.

## Development

```sh
go test -race ./...            # core + capi + server tests
nteedb-js/capi/build.sh        # rebuild the native libs (macOS host + Linux via Docker)
cd nteedb-js && npm test       # Node binding tests
```

## License

[Apache-2.0](LICENSE)

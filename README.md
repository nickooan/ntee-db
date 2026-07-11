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
- **Flexible index schema** — add, drop, or change indexes between opens with
  no migration step; dropped indexes are soft-deleted (data preserved,
  recoverable), and `Reindex` back-fills new indexes over existing records
  (see below).
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

| Directory                          | What it is                                                                                                                                                                                |
| ---------------------------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| [`nteedb-core/`](nteedb-core/)     | The store itself — a Go library (`package nteedb`). Full design & API docs in its [README](nteedb-core/README.md).                                                                        |
| [`nteedb-js/`](nteedb-js/)         | [`ntee-db`](nteedb-js/README.md), the Node.js binding — the core compiled as a c-shared library (source in `nteedb-js/capi/`), loaded via FFI, shipped with prebuilt binaries.            |
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
import { NteeDB } from "ntee-db";

const db = NteeDB.open("/path/to/store", {
  indexes: [{ name: "traceId", kind: "string" }],
});

db.put("call:1", { kind: "request" }, { traceId: "T1" });
const rec = await db.get("call:1"); // parsed JSON
await db.secIndex("traceId", "T1"); // ['call:1', ...]
db.close();
```

Prebuilt binaries ship for **darwin-arm64, linux-amd64, linux-arm64** — no Go
toolchain needed at install time. See
[`nteedb-js/README.md`](nteedb-js/README.md) for the API, benchmarks vs
`lmdb`/`better-sqlite3`, and notes.

## Evolving the index schema

ntee-db treats the index set as a declaration, not a migration: change
`Options.Indexes` (Go), the `indexes` open option (JS), or `schema.json`
(server) between opens and the store **adopts the new set — never rejected**:

- **Dropped index** → _soft-dropped_: it stops being maintained and queryable,
  but its data is **preserved in the records** (a tombstone entry in
  `meta.json` tracks it, and `Compact` deliberately keeps the data).
  Re-declare the index before a `Reindex` and its surviving data comes back.
- **Added (or kind-changed) index** → _prospective_: it covers records written
  from now on, but not history. `DroppedIndexes()` / `ProspectiveIndexes()`
  (server: `dropped` / `prospective`) report both states.
- **`Reindex()`** (server: `reindex`, admin) resolves everything at once: it
  rewrites every live record, **re-running each index's extractor (Go
  `Extract` func / JS & server `jsonPath`) over the old values** — so a newly
  added derived index gets back-filled across all existing data — and purges
  soft-dropped leftovers from records and `meta.json`. Reads stay live while
  it runs; writes wait.

The one asymmetry: only extractor-based indexes can be back-filled. An
explicit-values index (values passed per write) stays prospective — its
historical values were never recorded anywhere to recover. If you expect to
add an index over a field later, storing that field in the record and using
`jsonPath`/`Extract` keeps that door open.

Day-to-day writes never pay for any of this: extraction runs once at write
time, the result is persisted in the record, and boots/compactions rebuild
indexes from those persisted values without re-running extractors. Details in
[`nteedb-core/README.md`](nteedb-core/README.md).

## Building the server

The server is pure Go (no cgo), so every platform cross-compiles from any
host in seconds — no Docker:

```sh
./build.sh                 # → bin/nteedb-server-{darwin-arm64,linux-amd64,linux-arm64}
./build.sh linux-arm64     # one target
```

Linux builds are fully static — `scp` and run, no runtime dependencies.

## Development

```sh
go test -race ./...            # core + capi + server tests
./build.sh                     # server binaries (pure Go, cross-compiles)
nteedb-js/capi/build.sh        # JS native libs (macOS host + Linux via Docker)
cd nteedb-js && npm test       # Node binding tests
```

## License

[Apache-2.0](LICENSE)

# nteedb — core Go package

A small, pure-Go embedded key-value store for local CLI/TUI apps.
**Log-structured**: an append-only JSONL file is the source of truth, with
in-memory indexes for fast lookups. Its only dependency is
[`tidwall/btree`](https://github.com/tidwall/btree) (MIT), the ordered index
structure behind the primary and secondary indexes.

Beyond plain get/put, it supports **secondary indexes** (string/number, multi-
value) with exact, **prefix**, and range queries — exact and prefix take a `±N`
limit (first/last N; grouped per value for prefix) — plus **automatic per-value
capping** (`MaxPerValue`, keep only the newest N records per index value) and
**range delete by primary key** for time-based pruning.

## Install

```sh
go get github.com/nickooan/ntee-db
```

The directory is `nteedb-core`, but the declared package name is `nteedb` — an
explicit import alias keeps that obvious at the import site:

```go
import nteedb "github.com/nickooan/ntee-db/nteedb-core"
```

## Design

```
<dir>/main.jsonl        append-only data log — source of truth (one JSON record per line)
<dir>/blobs.dat         append-only large values, referenced by a blob ref in a main.jsonl line
<dir>/main.jsonl.hint   persisted index snapshot (JSONL): sorted key→{off,len} + a "covers" watermark
<dir>/LOCK              single-writer guard (kernel flock; the file itself carries no state)
```

- **No separate WAL.** The data log _is_ the write-ahead log: the index is always
  rebuildable from it and can never drift out of sync with the data.
- **Readable values.** Valid-UTF-8 values are stored as plain JSON strings
  (grep-able in `main.jsonl`, ~25% smaller); only genuine binary falls back to
  base64. Auto-detected — `utf8.Valid` is exactly the "can JSON carry it
  losslessly" predicate, so no corruption is possible in either direction.
  (Logs from older versions read fine; older binaries see new-format values as
  empty — acceptable for a best-effort store.)
- **In-memory index** is an ordered B-tree keyed by primary key
  (`tidwall/btree`). It serves exact lookups, prefix scans, and secondary-index
  range/prefix walks in O(log n) via seek + in-order iteration. (A hash map is
  avoided because prefix/range scans on a hash map require a full O(n) scan.) The
  B-tree's O(1) copy-on-write clone is also what makes the periodic hint snapshot
  cheap — see `HintEveryN`.
- **Hybrid memory.** Only the index (keys + offsets) is resident; record bodies
  and large values stay on disk and are read on demand. Values at or above
  `BlobThreshold` go to `blobs.dat` so the main log stays small.
- **Fast boot.** Startup loads the hint and replays only the log tail past its
  `covers` watermark; a missing/corrupt hint safely falls back to a full scan.
  A torn final line from a crash mid-append is detected and truncated.
- **Compaction** rewrites the main log with only live records (dropping
  superseded versions and tombstones) via a single atomic rename. **Reads stay
  live during the rewrite**: Compact/Reindex/BlobsRelieve raise only an internal
  compaction gate for the long rebuild (writes pend until it finishes) and take
  the exclusive lock just for the final swap. `LiveBytes()` vs
  `Stats().MainBytes` gives the dead-space ratio for deciding when to compact.
- **`BlobsRelieve()` reclaims blob space; `BlobUsage()` measures it.** Plain
  Compact never touches the blob file, so orphaned blobs (overwritten/deleted
  large values) accumulate. `BlobsRelieve` rewrites every live blob into a new
  generation file (`blobs.<n>.dat`), dropping the orphans — unconditionally:
  the core ships the *mechanism*, and the caller (a server's policy loop,
  application code) decides *when*, typically from `BlobUsage()`
  (total/live/orphaned bytes + generation count; O(records) small reads).
  Each blob ref names its generation, so the main-log rename stays the single
  atomic commit point — a crash on either side of it leaves a consistent store
  plus at most one stray unreferenced file, consolidated by the next
  BlobsRelieve. It compacts the main log in the same pass (the rewritten refs
  live there).
- **Single writer per store.** `Open` takes an exclusive, non-blocking kernel
  lock (`flock`) on `<dir>/LOCK`; a second process gets `ErrLocked` and can
  degrade gracefully (e.g. run without its cache). The lock is tied to the
  process, not the file — it releases automatically on any exit (Ctrl+C, crash,
  `kill -9`), so no stale-lock state is possible. Unix (macOS/Linux) only.

## Scope

Supports exact lookup and **prefix** search on the primary key — no
substring/fuzzy search.

## Key design & ordering

Primary keys are plain strings ordered **lexically**, and several features
derive their time semantics from that order — the store itself never tracks
insertion time. Design keys so that lexical order equals arrival order, e.g. a
namespace plus a **zero-padded, monotonically increasing** suffix
(`api:0000000000000123`):

- `RemoveByPkLess(cutoff)` / `RemoveByPkGreater(cutoff)` — range-delete every
  key strictly below/above a cutoff ("drop everything older than…").
- `ByIndex(name, val, -N)` / `ByIndexPrefix(name, prefix, -N)` — negative limits
  return the **last** |N| primary keys (newest-first when keys encode time).
- `MaxPerValue` eviction — the "oldest" record of an over-cap index value is
  the **lowest primary key** in its group.

With non-time-ordered keys these features still work, but "oldest/newest" means
"smallest/largest key" — so pick key shapes deliberately.

## Secondary indexes

Declare named secondary indexes (`string` or `number`) to look records up by
attributes other than the primary key, including **multi-value** indexes where
many records share a value (e.g. `traceId → many records`):

```go
db, _ := nteedb.Open(nteedb.Options{
    Dir: "/path/to/store",
    Indexes: []nteedb.IndexDef{
        {Name: "traceId", Kind: nteedb.KindString},
        {Name: "status",  Kind: nteedb.KindNumber},
        // Optional Extract derives the value from the record ("scan itself"):
        {Name: "kind", Kind: nteedb.KindString,
         Extract: func(key string, value []byte) (any, bool) { /* parse value */ }},
    },
})

db.PutIndexed("call:1", body, nteedb.IndexValues{"traceId": "T1", "status": 200})
db.Put("r1", jsonBody) // indexes with Extract derive their values automatically

db.ByIndex("traceId", "T1")         // → all primary keys with traceId T1
db.ByIndex("traceId", "T1", -1)     // → just the last (newest) key; +N = first N
db.ByIndexRange("status", 200, 299) // → keys in a numeric range
db.ByIndexPrefix("traceId", "Get")  // → string-prefix match
db.ByIndexPrefix("traceId", "Get", -1) // → the newest key of EACH matching value
```

`ByIndex` takes an optional limit: `0` (or omitted) = all matches ascending,
`N>0` = first N, `N<0` = last |N| descending. `ByIndexPrefix`'s limit is
**grouped per distinct value** — `-1` returns the newest record of every value
matching the prefix (e.g. the latest call per endpoint).

Index values are persisted in each record (and in the hint), so indexes are
rebuilt at boot from the small main-log lines / hint alone — no value or blob
reads. Each primary-index entry carries its record's current index values, so an
overwrite/delete retracts the stale secondary entries directly (no side map).
Compaction preserves all secondary lookups.

Simpler grouping (without declaring an index) can also be done by **namespacing
keys** (e.g. `input:`, `api:`) and prefix-scanning the primary key.

### Capping records per value (`MaxPerValue`)

An index can cap how many records share one value. When a write pushes a
value's group over the cap, the **oldest** record(s) — lowest primary key in the
group (see "Key design & ordering") — are evicted automatically:

```go
Indexes: []nteedb.IndexDef{
    // Keep at most the 5 newest records per endpoint value:
    {Name: "endpoint", Kind: nteedb.KindString, MaxPerValue: 5},
},
```

Eviction is a **full, durable delete**, identical to `Delete(pk)`: a tombstone
is appended to the log, and the record leaves the primary index and **every**
secondary index (a record evicted via its `endpoint` cap also disappears from
its `traceId` index, for example). `0` or omitted = unlimited.

Enforcement happens on the write path only. Boot replay and `Reindex` don't
evict — but if a group exceeds its cap (e.g. after lowering the cap between
opens), the next write to that value drains the whole excess. Overwriting an
existing key never triggers eviction (the group doesn't grow).

### Changing the index set

The declared schema is persisted in `meta.json`. Changing `Options.Indexes`
between opens is **never rejected** — the new set is adopted:

- A **dropped** index (removed from `Options`) is _soft-dropped_: it stops being
  maintained and isn't usable, but its existing `ix` data is **preserved** (a
  tombstone is kept in `meta`, and `Compact()` keeps the data). `db.DroppedIndexes()`
  lists soft-dropped indexes still lingering in records. `Reindex()` is what
  purges them completely — from both records and `meta`. Re-adding a dropped
  index before a `Reindex` recovers its surviving data.
- An **added** index is **prospective** — it covers records written _after_ it
  was added, but not older ones. `db.ProspectiveIndexes()` lists indexes that
  don't yet cover historical records.
- To back-fill an added index over existing records, call **`db.Reindex()`**.
  This re-runs each index's `Extract` over every record (reading values/blobs;
  O(table), one-time). Only `Extract`-based indexes can be back-filled —
  explicit-value indexes have no historical source and stay prospective.

### Dropping the store

```go
db.Drop()          // close + delete all of this store's files
nteedb.Destroy(dir) // same, by directory, when no DB is open
```

## Usage

```go
import nteedb "github.com/nickooan/ntee-db/nteedb-core"

db, err := nteedb.Open(nteedb.Options{Dir: "/path/to/store"})
if err != nil { /* ... */ }
defer db.Close()

db.Put("input:GetOrders", []byte("..."))
v, ok, err := db.Get("input:GetOrders")
keys, err := db.PrefixScan("input:Get") // sorted keys with that prefix
db.Delete("input:GetOrders")

// Bulk writes: one lock acquisition, one fsync in durable mode. Items apply in
// order; index values are validated up front, so a bad item writes nothing.
err = db.PutBatch([]nteedb.PutItem{
    {Key: "call:1", Value: body1, IX: nteedb.IndexValues{"traceId": "T1"}},
    {Key: "call:2", Value: body2},
})

// Range delete by primary key (strict bounds; the cutoff key itself is kept).
// With time-ordered keys this is "drop everything older/newer than X".
// Tombstone-based and crash-safe; run Compact() to reclaim the disk space.
n, err := db.RemoveByPkLess("api:0000000000000123")   // delete every key < cutoff
n, err = db.RemoveByPkGreater("api:0000000000000456") // delete every key > cutoff

db.Compact()
```

### Counters

`Incr` provides atomic int64 counters (decrement = negative delta):

```go
v, err := db.Incr("hits", 1)  // missing key initializes to 0 first → v == 1
v, err = db.Incr("hits", -5)  // v == -4
v, err = db.Incr("hits", 0)   // read without changing
```

Counters are int64-only, stored as a fixed-width value (explicit sign + 19
zero-padded digits — every int64 has exactly one such form). That fixed width
is what lets an increment overwrite the digits **in place** at the same file
offset: no log growth, no compaction pressure, and torn-write safe (a crash
mid-write can only mix old/new digit characters, never corrupt the line).
When an `Extract`-based index is declared, increments fall back to normal
appends so index derivation stays correct.

Counters are a distinct value type: `Incr` on a key holding any other value
returns `ErrNotCounter`, a `Put` on a counter key demotes it to a plain value,
and exceeding the int64 range returns `ErrCounterOverflow` with the value
unchanged. Fractional counting is deliberately unsupported (floats have no
bounded canonical text form) — store scaled integers instead (cents, not
dollars).

### Options

| Field            | Meaning                                                                                                                                                                                                                                                                                                                                                     |
| ---------------- | ---------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Dir`            | Store directory (required).                                                                                                                                                                                                                                                                                                                                 |
| `BlobThreshold`  | Values ≥ this many bytes go to `blobs.dat`. `0` → 64 KiB default; negative disables blobs. This is a layout/compaction knob, not a memory one (values are never resident regardless). Keep it generous: inline values are reclaimed by every `Compact`, whereas orphaned blobs are only reclaimed when the caller runs `BlobsRelieve` — so reserve blobs for genuinely large payloads. |
| `SyncEveryWrite` | fsync the log on every write (durable but slower). When false, a crash may lose the most recent writes.                                                                                                                                                                                                                                                     |
| `HintEveryN`     | Rewrite the hint after N writes (also on `Close` and after compaction). `0` disables periodic rewrites. Periodic rewrites run in a **background goroutine, off the write path** — a `Put` only pays a cheap in-memory snapshot; `Close`/`Compact`/range-delete hints remain synchronous checkpoints.                                                        |

## Testing

```sh
go test -race ./...
go test -bench . ./...
```

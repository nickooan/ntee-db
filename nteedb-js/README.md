# ntee-db (Node.js binding)

In-process Node.js binding for **nteedb** — a pure-Go embedded log-structured
KV store with prefix search and secondary indexes. The Go core is exposed as a
C-shared library and loaded via [koffi](https://koffi.dev) (FFI). No separate
process; same model as `lmdb`/`better-sqlite3` (prebuilt native binaries per
platform).

## Performance

Microbenchmark vs `lmdb` (lmdb-js) and `better-sqlite3` from Node, on a
cache-shaped workload: 20,000 records, ~120-byte JSON values, time-ordered keys
(`api:<zero-padded-id>`). Apple M2 Pro, Node 24; each figure is the mean of 5
rounds (fresh store per round, warm-up discarded). Scripts in
[`bench/`](bench/); **bold** marks the fastest engine per row.

| Operation                            | ntee-db    | lmdb            | better-sqlite3   |
| ------------------------------------ | ---------- | --------------- | ---------------- |
| `get`                                | 4.8 µs     | **1.1 µs**      | 1.5 µs           |
| exists check                         | 1.3 µs     | **0.7 µs**      | 1.6 µs           |
| put — fast path (caller-sync)        | **4.6 µs** | 1.6 µs (async†) | 10.9 µs          |
| put — fsync every write (power-loss) | ~3 ms      | ~3 ms           | ~3 ms            |
| batch (one 20k commit)               | 4.7 µs     | —               | **2.5 µs** (txn) |
| put — fast, `hintEveryN: 5`          | 16.2 µs    | —               | —                |
| prefix scan, all 20k keys            | **2.2 ms** | 3.9 ms          | 4.8 ms           |

† lmdb's fast path is **async/batched** — the write is not durable when the
call returns, so it is not a caller-synchronous write like ntee-db's `put` or
SQLite's. It is listed for context, not as a like-for-like peer in that row.

**On the two write tiers.** A per-write, power-loss-durable commit is bounded
by the hardware `fsync`, so all three engines land at ~3 ms — the engine barely
matters. That is exactly why r1quest uses the **fast path** instead: an
append-only write that is durable the moment the call returns (survives a
process crash; a power loss can lose only writes from the last fraction of a
second). ntee-db's append is the fastest of the caller-synchronous fast writes.
The fsync row uses matched durability: SQLite runs `synchronous=FULL` +
`fullfsync=ON` (its default `FULL` on macOS is ~35 µs but uses a lighter
`fsync` that does not flush the drive cache); ntee-db uses `syncEveryWrite`;
lmdb uses `putSync`.

**Indexed workload** — the app's real shape: every write carries two secondary
index values (`endpoint`, `traceId`); 20k records across 500 distinct
endpoints. SQLite is a genuine peer here (it has real secondary indexes); lmdb
has none, so its "latest per endpoint" is what the pre-ntee-db app code did —
full scan + parse + dedup in JS.

| Operation                           | ntee-db    | lmdb                   | better-sqlite3      |
| ----------------------------------- | ---------- | ---------------------- | ------------------- |
| put carrying 2 index values         | **9.0 µs** | 1.2 µs (no indexes\*)  | 25.2 µs             |
| put, full app config\*\*            | 62 µs      | —                      | —                   |
| search a value → keys (~20 matches) | 4.5 µs     | —§                     | **2.9 µs**          |
| search a value → records (~20)      | ~93 µs     | —§                     | **11 µs**           |
| latest call of one endpoint         | 3.4 µs     | —                      | **1.5 µs**          |
| latest call of every endpoint (500) | **0.2 ms** | 17.8 ms (scan + dedup) | 1.5 ms (`GROUP BY`) |

\* not equivalent work: lmdb's put maintains no indexes — that cost lands on
every query instead (the 17.8 ms row). ntee-db and SQLite both maintain the two
indexes on write.

§ lmdb has no secondary index — an equality search is a full scan + filter
(≈ the 17.8 ms scan) per value.

\*\* the app's exact open options (2 indexes + `maxPerValue: 5` on `endpoint` +
`hintEveryN: 5`): ~17.5k automatic durable evictions holding every endpoint at
its 5 newest records. Neither lmdb nor SQLite has native per-key retention (a
SQLite equivalent would need a trigger or periodic `DELETE`).

How to read this honestly:

- **Point reads: lmdb and SQLite both beat ntee-db (~3–4×).** ntee-db is the
  slowest reader — an FFI crossing + `pread` + JSON envelope vs a memory-mapped
  B+tree read or a prepared SQLite statement. All three are single-digit µs, so
  it is imperceptible for the tens of reads a user interaction makes.
- **Caller-synchronous writes: ntee-db is fastest (~2.4× vs SQLite).** Among
  writes that are durable-vs-crash the instant the call returns, the append-only
  log beats SQLite's B-tree page updates + WAL frame; lmdb's faster number is
  its async path, which is not caller-synchronous.
- **Power-loss-durable writes: a wash (~3 ms, all three).** `fsync` dominates,
  so this tier is not an engine differentiator — it is the reason r1quest keeps
  the fast append path as its default.
- **Batches: SQLite's transaction is fastest per-op, but blocks the JS thread.**
  ntee-db's `putMany` (4.7 µs/op) runs off the event loop and is one `fsync` in
  durable mode — a structural win (no thread stall, one sync for the whole
  load), not a per-op one.
- **Scans: ntee-db wins (~2×).** Keys live in a RAM-resident sorted index; a
  full-prefix scan is a bounded native traversal.
- **Grouped "newest per value": ntee-db wins even vs SQLite (~8×; ~90× vs
  lmdb).** `secIndexPrefix(name, prefix, -1)` returns one key per distinct value
  by seeking group-to-group through the index (skipping each group's interior),
  vs SQLite's `GROUP BY` or lmdb's scan-and-dedup in JS. This is the app's
  History-list query.
- **Record-returning index search is ntee-db's soft spot (~7× vs SQLite).**
  Looking up an index value's _keys_ is competitive (3–5 µs), but fetching the
  values adds the cost. `secIndexRecords` now uses a batched native `getMany`
  (one crossing, not N+1 — down from ~116 µs) and returns parsed objects
  directly, so it's ~93 µs for a 20-match value vs SQLite's 11 µs. The residual
  gap is **not** crossing count anymore —
  it's the koffi string boundary itself (Go marshals one JSON document, JS
  parses it), which only a native N-API addon would remove. Still sub-ms for the
  app's ~50-record renders.
- **Where the others fundamentally win:** SQLite brings SQL, transactions,
  multi-process access, and decades of durability hardening; lmdb brings the
  fastest reads and scales to datasets far past what ntee-db (all keys in RAM,
  O(n) boot scan — ~100 ms at 100k keys) is built for.

### Why r1quest uses ntee-db

It's a local, single-user app, so all three engines are "fast enough" — what
matters is _fit_. Every operation r1quest leans on is a row ntee-db wins in the
tables above:

| the app needs…                                    | ntee-db  | vs the field                                 |
| ------------------------------------------------- | -------- | -------------------------------------------- |
| persist before a one-shot CLI exits (sync write)  | ~9 µs    | ~2.8× faster than SQLite's caller-sync write |
| the History list — latest call per endpoint       | 0.2 ms   | ~8× SQLite `GROUP BY`, ~90× an lmdb scan     |
| a cache that can't grow (`maxPerValue` retention) | built-in | no native equivalent in SQLite or lmdb       |

Point reads are ~3× behind SQLite but sub-ms and rare per interaction. SQLite
would win if the app needed SQL, transactions, or multi-process; lmdb for raw
reads or far larger datasets — none of which a local request cache does. The one
real multi-process case (a TUI session overlapping a one-shot run) is guarded by
a single-writer `flock`: the second opener fails fast and degrades to a
cache-less run, and the lock releases on any exit (Ctrl+C, crash, `kill -9`).

### Running the benchmarks

`lmdb` and `better-sqlite3` are devDependencies (never shipped to consumers), so
`npm install` is all the setup needed:

```sh
node bench/core.mjs      # main table (ntee-db · lmdb · sqlite)
node bench/indexed.mjs   # indexed workload table
node bench/batch.mjs     # putMany, ntee-db only
node bench/parallel.mjs  # sequential vs Promise.all scans (libuv parallelism)

# parallel.mjs doubles as a fan-out stress test — 400 concurrent scans
# exercises the binding's internal queue (past koffi's 256-call cap):
BENCH_GROUPS=400 BENCH_PER_GROUP=500 node bench/parallel.mjs
```

## Usage

```js
import { NteeDB } from "ntee-db"

const db = NteeDB.open("/path/to/store", {
  blobThreshold: 64 * 1024, // values >= this go to the blob side file
  indexes: [
    { name: "traceId", kind: "string" }, // explicit values (passed per write)
    { name: "kind", kind: "string", jsonPath: "kind" }, // derived from the record — runs on every write, see Notes
  ],
})

// write — an object is JSON-serialized for you (3rd arg = explicit index values)
db.put("call:1", { kind: "request" }, { traceId: "T1" })

// read content back — ntee-db is a JSON store, so reads return the parsed value.
// Reads run off the event loop (a libuv worker), so they're async and concurrent
// reads run in parallel — await, or Promise.all to fan out.
const rec = await db.get("call:1") // { kind: "request" } | null
await db.getMany(["call:1", "call:2"]) // aligned to keys

// search → keys (async, off the loop); record-returning searches too
await db.secIndex("traceId", "T1") // ['call:1', ...]
await db.secIndexRecords("kind", "request") // [{ key, value }, ...]
await db.prefixScan("call:") // sorted keys
await db.secIndexRange("status", 200, 299) // numeric range

// concurrent scans run in parallel on libuv worker threads (RLock admits many
// readers); raise UV_THREADPOOL_SIZE (default 4) to use more cores. Fan out as
// wide as you like — the binding queues internally (see "Concurrency" below).
const [a, b] = await Promise.all([db.prefixScan("a:"), db.prefixScan("b:")])

// maintenance (off the event loop)
await db.compact() // reclaim dead records
await db.reindex() // back-fill jsonPath indexes over history; purge dropped
await db.blobUsage() // { totalBytes, liveBytes, orphanedBytes, generations }
await db.relieve() // rewrite blobs: drop orphaned ones + compact the main log
await db.details() // stats() + liveBytes + the blob breakdown

db.close() // or db.drop() to delete the store
```

### Values are JSON

ntee-db is a JSON store: reads return the value **parsed**. Whether you `put` a
string or a Buffer doesn't matter — what matters is whether the bytes are valid
JSON. Valid JSON comes back parsed; anything else comes back as a `Buffer`.

```js
// Store an object (JSON-serialized for you) → read it back parsed.
db.put("obj", { ok: true })
await db.get("obj") // → { ok: true }

// A stored scalar coerces per JSON parse rules.
db.put("n", "123")
await db.get("n") // → 123  (the number, not the string "123")

// Want to store NON-JSON content? Put a Buffer. It reads back as a Buffer,
// byte-exact. (If you pass a non-JSON string, ntee-db stores it fine and still
// hands it back as a Buffer — but a Buffer makes the intent explicit.)
db.put("blob", Buffer.from([0xff, 0x00, 0x01]))
db.put("text", "hello") // not valid JSON
await db.get("blob") // → <Buffer ff 00 01>
await db.get("text") // → <Buffer 68 65 6c 6c 6f>

// So: a Buffer coming out means the value was NOT JSON. Guard with Buffer.isBuffer.
const v = await db.get("some-key")
if (Buffer.isBuffer(v)) {
  // raw/binary value (or a corrupt record) — handle as bytes
} else {
  // parsed JSON: object / array / scalar (or null if the key is absent)
}
```

### Concurrency

All async methods run off the event loop (koffi async → a libuv worker), and
the Go store takes only a read lock for reads — so concurrent reads and scans
run in parallel on real cores. Parallelism is capped by the libuv pool
(`UV_THREADPOOL_SIZE`, default 4); raise it to use more cores.

**Fan out as wide as you like — the binding handles the concurrency for you.**
koffi (the FFI layer) allows at most 256 async calls in flight per process;
the binding gates in-flight FFI calls at 200 and FIFO-queues the rest
internally, so this just works:

```js
// 100k concurrent ops: 200 in flight at a time, the rest wait their turn —
// no "Too many asynchronous calls are running" error, ever.
const results = await Promise.all(keys.map((k) => db.get(k)))
```

What that costs and guarantees:

- **Queued ops are plain pending promises** — no timers, no polling, nothing
  extra keeping the event loop alive. Dispatch order is strictly FIFO.
- **Memory stays flat**: a waiting op costs ~0.5 KB of heap (measured:
  200k concurrent `get`s ≈ +106 MB while queued, fully reclaimed on settle),
  and the queue's own bookkeeping compacts as it drains. The native side is
  capped at 200 × 256 KiB regardless of how many ops you fire.
- **Errors stay per-op**: a rejected call releases its slot and rejects only
  its own promise; the rest of the batch is unaffected.
- Below 200 in flight there is no queue hop at all — calls dispatch
  synchronously, identical to a queue-less binding.

For truly huge batch jobs (millions of ops), chunk your `Promise.all` — not
for the queue's sake, but because a million pending promises is a caller-side
cost no library can absorb.

### Rate limiting (single application)

Counter ops + create-only TTL make a complete fixed-window limiter in one
atomic call — no read-then-write race, no cron to reset windows. The embedded
store is single-writer (one process per directory), so this fits a limiter
inside **one application process** — e.g. a Fastify/Express middleware. To
share limits across processes or hosts, run `nteedb-server` and use the same
calls from `ntee-db-client` instead.

Count **down from 0** with `take` and put the pool in the floor:

```js
// pool of 1000 tokens per user per minute; a missing/expired key counts as 0,
// so the counter runs 0 → -1000 and the floor enforces the pool atomically.
// cost = what the request consumes (e.g. "create N records" costs N);
// use cost 1 for a plain requests-per-minute limit.
const ok = await db.take(`rl:${userId}`, cost, -1000, 60_000)
if (!ok) reject() // not enough tokens left this window — nothing was written
```

The TTL is create-only: the first accepted take arms the 60s deadline, takes
on the live counter never slide it, and once it lapses the next accepted take
restarts the counter with a fresh window.

`take` is all-or-nothing: a refused take writes nothing (so it never arms a
window either), a successful one creates the counter and starts the window.
Tokens left = `1000 + value`; read the value with `get` if you need it for
logging (not `incr(key, 0)` — that would create an immortal counter at 0 and
the window could never arm). Verified under load: ~1.8M concurrent requests
against a Fastify middleware built on this pattern accepted exactly 1000
tokens per user per window, every window.

## API

| Method                                                        | Returns                                    | Notes                                                                                                                                    |
| ------------------------------------------------------------- | ------------------------------------------ | ---------------------------------------------------------------------------------------------------------------------------------------- |
| `NteeDB.open(dir, opts?)`                                     | `NteeDB`                                   | creates if missing                                                                                                                       |
| `NteeDB.destroy(dir)`                                         | `void`                                     | delete a store's files (no open handle)                                                                                                  |
| `put(key, value, ix?, ttlMs?)`                                | `void`                                     | `value`: object\|string\|Buffer (object → JSON); `ix`: `{name: string\|number}`; optional TTL (a put without one clears an existing TTL) |
| `putMany(items)`                                              | `Promise<number>`                          | one batch off the event loop; in-order; all-or-nothing validation                                                                        |
| `incr(key, delta?, ttlMs?)` / `decr(key, delta?, ttlMs?)`     | `Promise<number>`                          | atomic int64 counter; delta defaults to 1; ttl applies on create only — `incr(w, 1, 60000)` is a fixed-window rate limiter; **async**    |
| `topup(key, amount, max, ttlMs?)`                             | `Promise<number>`                          | fill toward `max`; resolves to the overflow that didn't fit (0 = fully applied); **async**                                               |
| `take(key, amount, left, ttlMs?)`                             | `Promise<boolean>`                         | subtract only if the result stays ≥ `left`; true iff applied; **async**                                                                  |
| `get(key)`                                                    | `Promise<value \| null>`                   | the stored JSON parsed (a Buffer for binary/non-JSON); **async** (off the loop)                                                          |
| `getMany(keys)`                                               | `Promise<(value\|null)[]>`                 | batched get, one crossing, aligned to `keys`; **async** (off the event loop)                                                             |
| `has(key)`                                                    | `Promise<boolean>`                         | **async** (off the loop)                                                                                                                 |
| `delete(key)`                                                 | `void`                                     |                                                                                                                                          |
| `stats()`                                                     | `Promise<{records, mainBytes, blobBytes}>` | cheap in-memory counters (sizes include dead space until `compact()`); **async**                                                         |
| `prefixScan(prefix)`                                          | `Promise<string[]>`                        | sorted keys; **async** — concurrent scans run in parallel (off the loop)                                                                 |
| `secIndex / secIndexPrefix / secIndexRange`                   | `Promise<string[]>`                        | primary keys; **async** (off the loop)                                                                                                   |
| `secIndexHas(name, val)`                                      | `Promise<boolean>`                         | any record has `val` in the index (no keys materialized); **async**                                                                      |
| `secIndexRecords / secIndexPrefixRecords / prefixScanRecords` | `Promise<{key, value}[]>`                  | keys + parsed content; **async** (record fetch off the event loop)                                                                       |
| `secIndexDropped / secIndexProspective`                       | `Promise<string[]>`                        | schema state; **async**                                                                                                                  |
| `compact()` / `reindex()`                                     | `Promise<void>`                            | run off the event loop                                                                                                                   |
| `relieve()`                                                   | `Promise<void>`                            | blob rewrite + compaction: reclaims orphaned blob space; O(live bytes incl. blobs); **async**                                            |
| `blobUsage()`                                                 | `Promise<BlobUsage>`                       | `{totalBytes, liveBytes, orphanedBytes, generations}` — when to `relieve()`; O(records), periodic use; **async**                         |
| `details()`                                                   | `Promise<StoreDetails>`                    | `stats()` + `liveBytes` + blob breakdown — the numbers a server operator sees; O(records); **async**                                     |
| `close()` / `drop()`                                          | `void`                                     |                                                                                                                                          |

## Notes / limitations

- **Counters**: `incr`/`decr` provide atomic int64 counters with Redis-style
  semantics — a missing key initializes to 0 before the delta applies
  (`incr(key, 0)` reads a counter, creating it at 0 if absent), the new value
  is returned, and `incr` on a key holding any other value rejects. Counters
  are primary-key-only (never in secondary indexes) and are updated **in
  place** — a hot counter never grows the log. Deltas must be safe integers;
  counter values beyond ±2^53 lose precision as JS numbers (the store itself
  keeps full int64 precision). `topup(key, amount, max)` and
  `take(key, amount, left)` are bounded updates for quota/stock patterns —
  the bound check and the write are one atomic operation, so concurrent
  callers can never overshoot. `topup` fills the counter toward `max` and
  resolves to the overflow that didn't fit (0 = the full amount applied; a
  counter already at/above max is left unchanged). `take` is all-or-nothing:
  it subtracts `amount` only if the result stays ≥ `left` and resolves
  `true` if applied. An operation that changes nothing writes nothing; the
  amount must be non-negative. These require a native library built from a
  version that exports them — rebuild via `capi/build.sh` if loading a
  locally built library.
- **TTL (lazy per-key expiry)**: every write takes an optional `ttlMs`;
  without it a key is immortal. Once the TTL passes the key reads as
  missing (`get` → null, `has` → false) and is deleted lazily in the
  background; `compact()`/`reindex()` drop leftovers. The rules:
  - `put` replaces the record wholesale, so its TTL state wins — `ttlMs`
    arms/replaces the expiry, omitting it **clears** an existing one.
  - Counter ops (`incr`/`decr`/`topup`/`take`) **never touch a live
    counter's TTL in either direction**: omitting `ttlMs` does NOT clear it
    (unlike `put`), and passing it on a live counter is ignored
    (create-only — a window's deadline never slides under traffic).
  - An **expired** counter is recreated fresh by the next op, and then the
    `ttlMs` arg decides: with it the new window is armed; without it the
    recreated counter is **immortal**. For expiring windows/buckets, pass
    `ttlMs` on every call — `take("rl:u1", cost, -1000, 60000)` is a complete
    fixed-window rate limiter (no-op on live keys, re-arms lapsed ones; see
    "Rate limiting" above).
  - Secondary indexes are TTL-unaware: an expired key can linger in
    `secIndex*` key results until cleanup; record fetches already drop it.
- **Only JSON-object values can be indexed**: immediate values — strings,
  numbers, booleans, arrays, binary — are plain key:value pairs addressed by
  primary key alone. Supplying `ix` with one throws, and `jsonPath` extraction
  skips them. Wrap such payloads in an object envelope if they must be indexed.
- **Index values from JS — explicit `ix` vs `jsonPath`**: supply them per write
  via `put(..., ix)`, or declare a `jsonPath` so the store derives the value from
  the record. They trade off:
  - **`jsonPath`** keeps the value in the record (no per-write `ix`) and is the
    only form `reindex()` can back-fill. Cost: the extractor runs on **every**
    write to the store, parsing each value to look for the field — so in a store
    with mixed record shapes, records that don't carry the field are parsed
    anyway (then skipped). Best when _all_ records share the indexed field.
  - **explicit `ix`** indexes only the records you pass it to and never parses
    the value — the better fit when only some record kinds carry the field. It
    cannot be back-filled by `reindex()` (past values were never recorded).
  - JS-function extractors aren't supported (a function can't cross the FFI
    boundary); `jsonPath` (a dotted path into the JSON value) is the declarative
    stand-in.
- **JSON store**: `put` takes a Buffer or string; **store JSON**. Reads return
  the value **parsed** — a stored scalar coerces (`put("k", "123")` reads back as
  the number `123`). A binary or non-JSON value (or a corrupt record) comes back
  as a `Buffer`, so callers can guard with `Buffer.isBuffer`. On disk, valid-UTF-8
  values are stored as plain JSON and only binary as base64.
- Errors from the store surface as `Error`s: synchronous methods (`put`, `delete`)
  throw; async methods (`get`, `has`, scans, `getMany`, `compact`, …) reject the
  returned promise. A call on a closed handle throws synchronously either way (the
  guard runs before the async hop).
- **Reads run off the event loop** (koffi async → a libuv worker thread), so the JS
  thread stays free and concurrent reads run in parallel — the Go store takes only a
  read lock, which admits many readers at once. Parallelism is capped by the libuv
  pool (`UV_THREADPOOL_SIZE`, default 4); raise it to use more cores.
- **Unbounded fan-out is safe** — the binding gates in-flight FFI calls at 200
  (headroom under koffi's process-wide 256-call cap) and FIFO-queues the rest,
  so `Error: Too many asynchronous calls are running` can no longer surface.
  Details and measured costs in ["Concurrency"](#concurrency) above.

## Building the native lib

The Go source for the c-shared library lives in [`capi/`](capi/) (it imports
the core from the repo's `nteedb-core/` package). Prebuilt binaries live in
`prebuilds/<os>-<arch>/`. To (re)build all three targets from macOS:

```sh
npm run build:native      # runs capi/build.sh → prebuilds/<os>-<arch>/
npm test
```

The script builds darwin on the host (requires Go) and both Linux arches in
the official `golang` Docker image via `--platform` (on Apple Silicon,
linux/amd64 runs under emulation). Individual targets:

```sh
capi/build.sh macos              # host build only
capi/build.sh linux-arm64        # one Linux arch (Docker)
```

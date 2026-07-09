// Indexed-workload benchmark: the app's real shape — every write carries two
// secondary index values; 20k records across 500 distinct endpoints.
//   - ntee-db: native secondary indexes.
//   - better-sqlite3: real SQL secondary indexes (a genuine peer here).
//   - lmdb: no secondary indexes, so "latest per endpoint" is scan+dedup in JS.
// Single pass; run a few times and average.
// Run from the package root: npm install && node bench/indexed.mjs
import { open as lmdbOpen } from "lmdb"
import Database from "better-sqlite3"
import { NteeDB } from "../src/index.js"
import { rmSync } from "node:fs"

const N = 20_000
const ENDPOINTS = 500
const endpoint = (i) => `/api/e${String(i % ENDPOINTS).padStart(3, "0")} [get]`
const traceId = (i) => `T${i % 1000}`
const key = (i) => `api:${String(i).padStart(16, "0")}`
const value = (i) =>
  JSON.stringify({
    endpoint: endpoint(i),
    status: 200,
    durationMs: 42,
    data: { id: i, name: "some user" },
  })

const report = (label, ms, ops) =>
  console.log(
    `${label}: ${ms.toFixed(1)} ms total, ${((ms * 1000) / ops).toFixed(1)} µs/op`,
  )

const rmSqlite = (base) => {
  for (const ext of ["", "-wal", "-shm"]) rmSync(base + ext, { force: true })
}

// --- ntee-db, with the app's two secondary indexes declared ---
rmSync("./ntee-ix", { recursive: true, force: true })
const ndb = NteeDB.open("./ntee-ix", {
  indexes: [
    { name: "endpoint", kind: "string" },
    { name: "traceId", kind: "string" },
  ],
})
{
  const t0 = performance.now()
  for (let i = 0; i < N; i++)
    ndb.put(key(i), value(i), { endpoint: endpoint(i), traceId: traceId(i) })
  report("ntee-db put (sync, 2 indexes)", performance.now() - t0, N)
}
{
  const t0 = performance.now()
  for (let i = 0; i < ENDPOINTS; i++)
    await ndb.secIndex("endpoint", endpoint(i), -1)
  report("ntee-db secIndex exact latest", performance.now() - t0, ENDPOINTS)
}
{
  const t0 = performance.now()
  const keys = await ndb.secIndexPrefix("endpoint", "/api/", -1)
  console.log(
    `ntee-db secIndexPrefix('/api/', -1) → latest of all ${keys.length} endpoints: ${(performance.now() - t0).toFixed(1)} ms`,
  )
}
{
  // Secondary-index equality search: all keys for a value (traceId → ~20 each).
  const t0 = performance.now()
  for (let i = 0; i < 1000; i++) await ndb.secIndex("traceId", `T${i}`)
  report("ntee-db secIndex all (search, keys)", performance.now() - t0, 1000)
}
{
  // Same search, returning records (keys + values) in one call.
  const t0 = performance.now()
  for (let i = 0; i < 1000; i++) await ndb.secIndexRecords("traceId", `T${i}`)
  report(
    "ntee-db secIndexRecords (search, records)",
    performance.now() - t0,
    1000,
  )
}
ndb.close()

// --- the app's FULL open config: 2 indexes + maxPerValue:5 + hintEveryN:5 ---
rmSync("./ntee-full", { recursive: true, force: true })
const nf = NteeDB.open("./ntee-full", {
  hintEveryN: 5,
  indexes: [
    { name: "endpoint", kind: "string", maxPerValue: 5 },
    { name: "traceId", kind: "string" },
  ],
})
{
  const t0 = performance.now()
  for (let i = 0; i < N; i++)
    nf.put(key(i), value(i), { endpoint: endpoint(i), traceId: traceId(i) })
  report("ntee-db put (FULL app config)", performance.now() - t0, N)
}
nf.close()

// --- better-sqlite3, with the two secondary indexes SQLite maintains ---
rmSqlite("./sqlite-ix.sqlite")
const sdb = new Database("./sqlite-ix.sqlite")
sdb.pragma("journal_mode = WAL")
sdb.pragma("synchronous = NORMAL")
sdb.exec(
  `CREATE TABLE calls (key TEXT PRIMARY KEY, value TEXT,
                       endpoint TEXT, traceId TEXT) WITHOUT ROWID`,
)
sdb.exec("CREATE INDEX ix_ep ON calls(endpoint, key)") // composite → latest-per-endpoint index-only
sdb.exec("CREATE INDEX ix_tr ON calls(traceId)")
{
  const put = sdb.prepare(
    "INSERT OR REPLACE INTO calls (key, value, endpoint, traceId) VALUES (?, ?, ?, ?)",
  )
  const t0 = performance.now()
  for (let i = 0; i < N; i++) put.run(key(i), value(i), endpoint(i), traceId(i))
  report("sqlite put (sync, 2 indexes)", performance.now() - t0, N)
}
{
  const q = sdb.prepare(
    "SELECT key FROM calls WHERE endpoint = ? ORDER BY key DESC LIMIT 1",
  )
  const t0 = performance.now()
  for (let i = 0; i < ENDPOINTS; i++) q.get(endpoint(i))
  report("sqlite latest one endpoint", performance.now() - t0, ENDPOINTS)
}
{
  const q = sdb.prepare(
    "SELECT endpoint, MAX(key) AS key FROM calls GROUP BY endpoint",
  )
  const t0 = performance.now()
  const rows = q.all()
  console.log(
    `sqlite GROUP BY → latest of all ${rows.length} endpoints: ${(performance.now() - t0).toFixed(1)} ms`,
  )
}
{
  // Secondary-index equality search: all keys for a value (served by ix_tr).
  const q = sdb.prepare("SELECT key FROM calls WHERE traceId = ?").pluck()
  const t0 = performance.now()
  for (let i = 0; i < 1000; i++) q.all(`T${i}`)
  report("sqlite search (keys)", performance.now() - t0, 1000)
}
{
  // Same search, returning records (key + value).
  const q = sdb.prepare("SELECT key, value FROM calls WHERE traceId = ?")
  const t0 = performance.now()
  for (let i = 0; i < 1000; i++) q.all(`T${i}`)
  report("sqlite search (records)", performance.now() - t0, 1000)
}
sdb.close()
rmSqlite("./sqlite-ix.sqlite")

// --- lmdb: same records; "latest per endpoint" = scan + parse + dedup in JS ---
rmSync("./lmdb-ix", { recursive: true, force: true })
const ldb = lmdbOpen({ path: "./lmdb-ix" })
{
  const t0 = performance.now()
  let last
  for (let i = 0; i < N; i++) last = ldb.put(key(i), value(i))
  await last
  report("lmdb put (async batched, no ix)", performance.now() - t0, N)
}
{
  const t0 = performance.now()
  const latest = new Map()
  for (const { value: v } of ldb.getRange({ start: "api:", end: "api;" })) {
    const rec = JSON.parse(v)
    latest.set(rec.endpoint, rec)
  }
  console.log(
    `lmdb scan+dedup → latest of all ${latest.size} endpoints: ${(performance.now() - t0).toFixed(1)} ms`,
  )
}
await ldb.close()
process.exit(0) // koffi keeps the event loop alive; force a clean exit

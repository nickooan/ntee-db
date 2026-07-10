// Main-table benchmark: ntee-db vs lmdb vs better-sqlite3 on a cache-shaped
// workload. Mean of 5 rounds, fresh store per round, warm-up round discarded.
// Writes are split into two honest tiers, compared like-for-like:
//   - fast path (caller-sync, no fsync): ntee-db default put · lmdb async
//     batched · sqlite WAL+NORMAL — all survive process crash, not power loss.
//   - fsync-durable (fsync every commit): ntee-db syncEveryWrite · lmdb putSync
//     · sqlite WAL+FULL — all survive power loss.
// Run from the package root: npm install && node bench/core.mjs
import { open as lmdbOpen } from "lmdb"
import Database from "better-sqlite3"
import { NteeDB } from "../src/index.js"
import { rmSync } from "node:fs"

const N = 20_000
const ROUNDS = 5
const value = JSON.stringify({
  endpoint: "/api/users [get]",
  status: 200,
  durationMs: 42,
  headers: { "content-type": "application/json" },
  data: { id: 123, name: "some user", tags: ["a", "b"] },
})
const key = (i) => `api:${String(i).padStart(16, "0")}`

const sums = new Map()

const cleanup = () => {
  for (const p of [
    "./ntee-store",
    "./ntee-sync",
    "./ntee-batch",
    "./ntee-hints",
    "./lmdb-store",
  ])
    rmSync(p, { recursive: true, force: true })
  for (const ext of ["", "-wal", "-shm"])
    for (const base of [
      "./sqlite-normal.sqlite",
      "./sqlite-full.sqlite",
      "./sqlite-batch.sqlite",
    ])
      rmSync(base + ext, { force: true })
}

const openSqlite = (path, sync) => {
  const db = new Database(path)
  db.pragma("journal_mode = WAL")
  db.pragma(`synchronous = ${sync}`) // NORMAL (fast) | FULL (fsync/commit)
  // On macOS a plain fsync doesn't flush the drive cache; fullfsync (F_FULLFSYNC)
  // does — the true power-loss guarantee that ntee-db syncEveryWrite and lmdb
  // putSync also pay. Enable it for FULL so the fsync row is apples-to-apples.
  if (sync === "FULL") db.pragma("fullfsync = ON")
  db.exec(
    "CREATE TABLE IF NOT EXISTS kv (key TEXT PRIMARY KEY, value TEXT) WITHOUT ROWID",
  )
  return db
}

async function round(warmup) {
  const time = async (label, fn) => {
    const t0 = performance.now()
    await fn()
    const ms = performance.now() - t0
    if (warmup) return
    if (!sums.has(label)) sums.set(label, [])
    sums.get(label).push(ms)
  }

  cleanup()

  // --- ntee-db ---
  const ndb = NteeDB.open("./ntee-store", {})
  await time("ntee-db put (fast, no fsync)", () => {
    for (let i = 0; i < N; i++) ndb.put(key(i), value)
  })
  // Reads are async now (off the event loop). This loop awaits each call
  // serially — honest single-caller latency, including the libuv thread hop.
  // The parallel upside (Promise.all across worker threads) is in bench/parallel.mjs.
  await time("ntee-db get", async () => {
    for (let i = 0; i < N; i++) await ndb.get(key(i))
  })
  await time("ntee-db has", async () => {
    for (let i = 0; i < N; i++) await ndb.has(key(i))
  })
  await time("ntee-db prefixScan all [total]", async () => {
    await ndb.prefixScan("api:")
  })
  ndb.close()

  // ntee-db fsync-durable write (syncEveryWrite: fsync every append).
  const nsync = NteeDB.open("./ntee-sync", { syncEveryWrite: true })
  await time("ntee-db put (fsync/write)", () => {
    for (let i = 0; i < N; i++) nsync.put(key(i), value)
  })
  nsync.close()

  // ntee-db batch: one FFI call, off the event loop.
  const nb = NteeDB.open("./ntee-batch", {})
  const items = Array.from({ length: N }, (_, i) => ({ key: key(i), value }))
  await time("ntee-db putMany (one batch)", () => nb.putMany(items))
  nb.close()

  // ntee-db boot-optimization config: periodic hints (async, background).
  const nh = NteeDB.open("./ntee-hints", { hintEveryN: 5 })
  await time("ntee-db put (fast, hintEveryN=5)", () => {
    for (let i = 0; i < N; i++) nh.put(key(i), value)
  })
  nh.close()

  // --- lmdb ---
  const ldb = lmdbOpen({ path: "./lmdb-store" })
  await time("lmdb put (fsync, putSync)", () => {
    for (let i = 0; i < N; i++) ldb.putSync(key(i), value)
  })
  await time("lmdb put (fast, async batched)", async () => {
    let last
    for (let i = 0; i < N; i++) last = ldb.put(key(i), value)
    await last // batched async transactions — lmdb-js's fast path
  })
  await time("lmdb get", () => {
    for (let i = 0; i < N; i++) ldb.get(key(i))
  })
  await time("lmdb doesExist", () => {
    for (let i = 0; i < N; i++) ldb.doesExist(key(i))
  })
  await time("lmdb prefixScan all [total]", () => {
    ;[...ldb.getKeys({ start: "api:", end: "api;" })]
  })
  await ldb.close()

  // --- better-sqlite3 (WAL + NORMAL = fast path) ---
  const sdb = openSqlite("./sqlite-normal.sqlite", "NORMAL")
  const sPut = sdb.prepare(
    "INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)",
  )
  const sGet = sdb.prepare("SELECT value FROM kv WHERE key = ?")
  const sHas = sdb.prepare("SELECT 1 FROM kv WHERE key = ?")
  const sScan = sdb.prepare(
    "SELECT key FROM kv WHERE key >= 'api:' AND key < 'api;' ORDER BY key",
  )
  const sPutMany = sdb.transaction((rows) => {
    for (const [k, v] of rows) sPut.run(k, v)
  })
  await time("sqlite put (fast, WAL NORMAL)", () => {
    for (let i = 0; i < N; i++) sPut.run(key(i), value)
  })
  await time("sqlite get", () => {
    for (let i = 0; i < N; i++) sGet.get(key(i))
  })
  await time("sqlite has", () => {
    for (let i = 0; i < N; i++) sHas.get(key(i))
  })
  await time("sqlite prefixScan all [total]", () => {
    sScan.all()
  })
  sdb.close()

  // better-sqlite3 fsync-durable write (WAL + FULL = fsync every commit).
  const sfull = openSqlite("./sqlite-full.sqlite", "FULL")
  const sfPut = sfull.prepare(
    "INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)",
  )
  await time("sqlite put (fsync, WAL FULL)", () => {
    for (let i = 0; i < N; i++) sfPut.run(key(i), value)
  })
  // batch: one transaction → one commit (fresh file for a fair insert time).
  const sbatch = openSqlite("./sqlite-batch.sqlite", "NORMAL")
  const sbPut = sbatch.prepare(
    "INSERT OR REPLACE INTO kv (key, value) VALUES (?, ?)",
  )
  const sbMany = sbatch.transaction((rows) => {
    for (const [k, v] of rows) sbPut.run(k, v)
  })
  const rows = Array.from({ length: N }, (_, i) => [key(i), value])
  await time("sqlite putMany (txn)", () => {
    sbMany(rows)
  })
  sfull.close()
  sbatch.close()

  cleanup()
  console.error(warmup ? "warmup round done" : "round done")
}

await round(true)
for (let r = 0; r < ROUNDS; r++) await round(false)

console.log(`\nmean of ${ROUNDS} rounds, N=${N}:`)
for (const [label, times] of sums) {
  const mean = times.reduce((a, b) => a + b, 0) / times.length
  console.log(
    label.includes("[total]")
      ? `${label}: ${mean.toFixed(1)} ms`
      : `${label}: ${((mean * 1000) / N).toFixed(1)} µs/op`,
  )
}
process.exit(0) // koffi keeps the event loop alive; force a clean exit

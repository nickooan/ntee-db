// Parallel-read benchmark: the payoff of async reads. Each read now runs off the
// event loop on a libuv worker thread, and the Go store takes only a read lock —
// so independent scans dispatched together run in parallel on multiple cores.
//
// This compares, for the SAME set of independent scans:
//   - sequential: `for (...) await db.prefixScan(g)`   (one worker at a time)
//   - parallel:   `await Promise.all(groups.map(g => db.prefixScan(g)))`
//
// Parallelism is capped by the libuv thread pool (UV_THREADPOOL_SIZE, default 4).
// Raise it to use more cores, e.g.:
//   UV_THREADPOOL_SIZE=16 node bench/parallel.mjs
//
// Rounds are INTERLEAVED (seq, par, seq, par, …) rather than all-seq-then-all-par:
// same-ordered runs on a warm/throttling laptop can invent a large phantom delta.
// Run from the package root: node bench/parallel.mjs
import { NteeDB } from "../src/index.js"
import { rmSync } from "node:fs"

const GROUPS = 8 // distinct prefixes → 8 independent scans per batch
const PER_GROUP = 25_000 // keys per prefix; big enough that a scan is real work
const ROUNDS = 6
const WARMUP = 1

const pool = Number(process.env.UV_THREADPOOL_SIZE) || 4 // libuv default is 4
const group = (g) => `p${String(g).padStart(2, "0")}:`
const key = (g, i) => group(g) + String(i).padStart(12, "0")
const value = JSON.stringify({ status: 200, durationMs: 42, note: "record" })

rmSync("./ntee-parallel", { recursive: true, force: true })
const db = NteeDB.open("./ntee-parallel", {})
{
  // One batched write to populate GROUPS × PER_GROUP records quickly.
  const items = []
  for (let g = 0; g < GROUPS; g++)
    for (let i = 0; i < PER_GROUP; i++) items.push({ key: key(g, i), value })
  await db.putMany(items)
}

const prefixes = Array.from({ length: GROUPS }, (_, g) => group(g))

async function sequential() {
  let total = 0
  for (const p of prefixes) total += (await db.prefixScan(p)).length
  return total
}

async function parallel() {
  const results = await Promise.all(prefixes.map((p) => db.prefixScan(p)))
  return results.reduce((n, keys) => n + keys.length, 0)
}

const time = async (fn) => {
  const t0 = performance.now()
  const n = await fn()
  return { ms: performance.now() - t0, n }
}

const seqMs = []
const parMs = []
for (let r = 0; r < WARMUP + ROUNDS; r++) {
  const s = await time(sequential)
  const p = await time(parallel)
  if (s.n !== p.n) throw new Error(`mismatch: seq=${s.n} par=${p.n}`)
  if (r < WARMUP) continue // discard warm-up
  seqMs.push(s.ms)
  parMs.push(p.ms)
}

db.drop()

const mean = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length
const seq = mean(seqMs)
const par = mean(parMs)

console.log(
  `\n${GROUPS} independent prefixScans of ${PER_GROUP} keys each, ` +
    `mean of ${ROUNDS} rounds, UV_THREADPOOL_SIZE=${pool}:`,
)
console.log(`  sequential (await in a loop): ${seq.toFixed(1)} ms`)
console.log(`  parallel   (Promise.all):     ${par.toFixed(1)} ms`)
console.log(
  `  speedup: ${(seq / par).toFixed(2)}× ` +
    `(bounded by min(${GROUPS} scans, ${pool} workers, cores))`,
)
process.exit(0) // koffi keeps the event loop alive; force a clean exit

// A/B: records-by-index in ONE native crossing (secIndexRecords) vs the old
// TWO-crossing shape (secIndex keys + getMany). Same store, interleaved rounds
// to cancel thermal/scheduler drift. Run: node bench/records-ab.mjs
import { NteeDB } from "../src/index.js"
import { rmSync } from "node:fs"

const N = 20_000
const TRACES = 1000 // ~20 records per traceId
const ROUNDS = 8
const WARMUP = 2

const key = (i) => `api:${String(i).padStart(16, "0")}`
const traceId = (i) => `T${i % TRACES}`
const value = JSON.stringify({ status: 200, durationMs: 42, note: "record" })

rmSync("./ntee-recab", { recursive: true, force: true })
const db = NteeDB.open("./ntee-recab", {
  indexes: [{ name: "traceId", kind: "string" }],
})
{
  const items = Array.from({ length: N }, (_, i) => ({
    key: key(i),
    value,
    ix: { traceId: traceId(i) },
  }))
  await db.putMany(items)
}

// New: single native crossing.
async function oneCrossing() {
  let n = 0
  for (let i = 0; i < TRACES; i++) {
    n += (await db.secIndexRecords("traceId", `T${i}`)).length
  }
  return n
}

// Old: keys query + separate batched getMany (two crossings per search).
async function twoCrossings() {
  let n = 0
  for (let i = 0; i < TRACES; i++) {
    const keys = await db.secIndex("traceId", `T${i}`)
    await db.getMany(keys)
    n += keys.length
  }
  return n
}

const time = async (fn) => {
  const t0 = performance.now()
  const n = await fn()
  return { ms: performance.now() - t0, n }
}

const one = []
const two = []
for (let r = 0; r < WARMUP + ROUNDS; r++) {
  const a = await time(oneCrossing)
  const b = await time(twoCrossings)
  if (a.n !== b.n) throw new Error(`mismatch ${a.n} vs ${b.n}`)
  if (r < WARMUP) continue
  one.push(a.ms)
  two.push(b.ms)
}

db.drop()

const mean = (xs) => xs.reduce((a, b) => a + b, 0) / xs.length
const oneUs = (mean(one) * 1000) / TRACES
const twoUs = (mean(two) * 1000) / TRACES
console.log(
  `\n${TRACES} record searches (~20 records each), mean of ${ROUNDS} rounds:`,
)
console.log(`  one crossing (secIndexRecords):     ${oneUs.toFixed(1)} µs/op`)
console.log(`  two crossings (secIndex + getMany): ${twoUs.toFixed(1)} µs/op`)
console.log(
  `  saved: ${(twoUs - oneUs).toFixed(1)} µs/op (${((1 - oneUs / twoUs) * 100).toFixed(0)}%)`,
)
process.exit(0)

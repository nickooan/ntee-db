// putMany benchmark: N records in one FFI call, off the event loop. Values ride
// a single concatenated buffer (no per-value escaping/base64), so the win over
// a JSON payload shows most with large or binary values.
// Single pass; run a few times and average.
// Run from the package root: node bench/batch.mjs
import { NteeDB } from "../src/index.js"
import { rmSync } from "node:fs"

const N = 20_000
const key = (i) => "api:" + String(i).padStart(16, "0")

async function bench(label, makeValue) {
  rmSync("./ntee-batch", { recursive: true, force: true })
  const db = NteeDB.open("./ntee-batch", { blobThreshold: 1 << 20 })
  const items = Array.from({ length: N }, (_, i) => ({
    key: key(i),
    value: makeValue(i),
  }))
  const t0 = performance.now()
  await db.putMany(items) // one FFI call, off the event loop
  console.log(
    `putMany ${label} (${N}): ${(((performance.now() - t0) * 1000) / N).toFixed(1)} µs/op`,
  )
  db.drop()
}

// ~120-byte JSON record (the app's shape).
const small = JSON.stringify({ endpoint: "/api/users [get]", status: 200 })
await bench("small JSON (~120 B)", () => small)

// 4 KiB text value — the JSON-payload path would escape all of it; the binary
// ABI copies it once, verbatim.
const bigText = "x".repeat(4096)
await bench("4 KiB text", () => bigText)

// 4 KiB binary value — the JSON path base64'd this (+33%); the binary ABI does
// not.
const bigBin = Buffer.alloc(4096, 0xab)
await bench("4 KiB binary", () => bigBin)

process.exit(0) // koffi keeps the event loop alive; force a clean exit

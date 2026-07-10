// Minimal end-to-end example: open, write with a secondary index, search,
// reopen, and read recovered state.
import { mkdtempSync, rmSync } from "node:fs"
import { tmpdir } from "node:os"
import path from "node:path"
import { NteeDB } from "./src/index.js"

const dir = mkdtempSync(path.join(tmpdir(), "nteedb-example-"))

let db = NteeDB.open(dir, {
  indexes: [
    { name: "traceId", kind: "string" },
    { name: "kind", kind: "string", jsonPath: "kind" }, // auto-derived from JSON
  ],
})

// Objects are JSON-serialized for you.
db.put("call:1", { kind: "request", url: "/orders" }, { traceId: "T1" })
db.put("call:2", { kind: "request", url: "/items" }, { traceId: "T1" })
db.put("call:3", { kind: "history", url: "/x" }, { traceId: "T2" })

console.log("by traceId T1:", await db.secIndex("traceId", "T1"))
console.log("by kind=request:", await db.secIndex("kind", "request"))
// Record-returning searches are async (the value fetch runs off the event
// loop); values come back already parsed.
console.log(
  "secIndexRecords kind=request:",
  await db.secIndexRecords("kind", "request"),
)

db.close()

// Reopen — state is recovered from disk.
db.close() // release the single-writer lock before reopening
db = NteeDB.open(dir, {
  indexes: [
    { name: "traceId", kind: "string" },
    { name: "kind", kind: "string" },
  ],
})
// Default json valueFormat → get returns the parsed record directly.
console.log("after reopen, get call:1:", await db.get("call:1"))
db.close()

rmSync(dir, { recursive: true, force: true })
console.log("done")

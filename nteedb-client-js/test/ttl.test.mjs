import { test } from "node:test"
import assert from "node:assert/strict"
import { buildPutex, buildPutx, ttlArg } from "../src/protocol.js"
import { withClient } from "./harness.mjs"

// Poll until fn() resolves truthy or ~2s pass (server TTLs are real time).
async function eventually(fn) {
  const deadline = Date.now() + 2000
  while (Date.now() < deadline) {
    if (await fn()) return true
    await new Promise((r) => setTimeout(r, 5))
  }
  return false
}

test("ttl frames and validation (unit)", () => {
  assert.deepEqual(
    buildPutex("k", 500, Buffer.from("abc")),
    Buffer.from("putex k 500 3\r\nabc\r\n"),
  )
  assert.deepEqual(
    buildPutx("k", Buffer.from("{}"), Buffer.from("{}"), 500),
    Buffer.from("putx k 2 2 500\r\n{}\r\n{}\r\n"),
  )
  // No ttl → the classic 3-arg frame, byte-identical to before.
  assert.deepEqual(
    buildPutx("k", Buffer.from("{}"), Buffer.from("{}")),
    Buffer.from("putx k 2 2\r\n{}\r\n{}\r\n"),
  )
  assert.equal(ttlArg(undefined), 0)
  assert.equal(ttlArg(1500), 1500)
  for (const bad of [0, -1, 1.5, NaN, "1000"]) {
    assert.throws(() => ttlArg(bad), TypeError)
  }
})

test("put with ttl expires; plain put clears a ttl", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("eph", { a: 1 }, undefined, 40)
    assert.deepEqual(await db.get("eph"), { a: 1 })
    assert.ok(
      await eventually(async () => (await db.get("eph")) === null),
      "ttl key never expired",
    )
    // Plain put clears a pending expiry.
    await db.put("k", { v: 1 }, undefined, 40)
    await db.put("k", { v: 2 })
    await new Promise((r) => setTimeout(r, 80))
    assert.deepEqual(await db.get("k"), { v: 2 })
    // Synchronous validation.
    assert.throws(() => db.put("x", "v", undefined, 0), TypeError)
    assert.throws(() => db.incr("x", 1, -5), TypeError)
  })
})

test("indexed put with ttl; ixrec self-heals after expiry", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("call:1", { kind: "x" }, { traceId: "T1" }, 40)
    assert.deepEqual(await db.secIndex("traceId", "T1"), ["call:1"])
    assert.ok(
      await eventually(async () => (await db.has("call:1")) === false),
      "indexed ttl key never expired",
    )
    // Records fetch drops the expired key even if the index still lists it.
    const rows = await db.secIndexRecords("traceId", "T1")
    assert.deepEqual(rows, [])
  })
})

test("fixed-window rate limiting over the wire", async () => {
  await withClient({}, {}, async (db) => {
    assert.equal(await db.incr("w", 1, 80), 1) // arms the window
    assert.equal(await db.incr("w", 1, 80), 2) // counts inside it
    assert.ok(
      await eventually(async () => (await db.incr("w", 1, 80)) === 1),
      "window never restarted",
    )
  })
})

test("topup/take create-only ttl", async () => {
  await withClient({}, {}, async (db) => {
    assert.equal(await db.topup("b", 5, 10, 50), 0)
    assert.equal(await db.take("b", 2, 0), true)
    assert.ok(
      await eventually(async () => (await db.take("b", 1, 0)) === false),
      "bucket never expired",
    )
    assert.equal(await db.topup("b", 3, 10, 50), 0) // recreated fresh
    assert.equal(await db.incr("b", 0), 3)
  })
})

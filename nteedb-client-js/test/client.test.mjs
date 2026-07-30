import { test } from "node:test"
import assert from "node:assert/strict"
import { NteeServerError } from "../src/index.js"
import { withClient } from "./harness.mjs"

test("connect populates info with server, auth mode, and indexes", async () => {
  await withClient({}, {}, async (db) => {
    assert.equal(db.info.server, "nteedb")
    assert.equal(db.info.auth, "none")
    assert.deepEqual(db.info.indexes.map((ix) => ix.name).sort(), [
      "kind",
      "status",
      "traceId",
    ])
    assert.equal(await db.ping(), "pong")
  })
})

test("put/get/has/delete round-trip", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("k1", { a: 1, s: "with spaces" })
    assert.deepEqual(await db.get("k1"), { a: 1, s: "with spaces" })
    assert.equal(await db.has("k1"), true)
    assert.equal(await db.get("nope"), null)
    assert.equal(await db.has("nope"), false)
    assert.equal(await db.delete("k1"), true)
    assert.equal(await db.get("k1"), null)
    // Deleting an absent key still resolves true (server semantics).
    assert.equal(await db.delete("k1"), true)
  })
})

test("getMany aligns to the requested keys", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("a", { n: 1 })
    await db.put("b", { n: 2 })
    assert.deepEqual(await db.getMany(["a", "missing", "b", "a"]), [
      { n: 1 },
      null,
      { n: 2 },
      { n: 1 },
    ])
    assert.throws(() => db.getMany([]), TypeError)
  })
})

test("prefixScan returns sorted keys; empty result is []", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("api:2", "x")
    await db.put("api:1", "x")
    await db.put("other", "x")
    assert.deepEqual(await db.prefixScan("api:"), ["api:1", "api:2"])
    assert.deepEqual(await db.prefixScan(), ["api:1", "api:2", "other"])
    assert.deepEqual(await db.prefixScan("zzz"), [])
  })
})

test("incr/decr counters", async () => {
  await withClient({}, {}, async (db) => {
    assert.equal(await db.incr("hits"), 1)
    assert.equal(await db.incr("hits", 41), 42)
    assert.equal(await db.decr("hits", 2), 40)
    assert.equal(await db.decr("hits"), 39)
    assert.equal(await db.incr("hits", 0), 39) // read idiom
    // get on a counter returns a Buffer (fixed-width form is not JSON).
    const raw = await db.get("hits")
    assert.ok(Buffer.isBuffer(raw))
    assert.equal(raw.toString(), "+0000000000000000039")
    // Type rule: incr on a plain value rejects with the server's text.
    await db.put("plain", { a: 1 })
    await assert.rejects(db.incr("plain"), NteeServerError)
    await assert.rejects(db.incr("plain"), /non-counter/)
    // Client-side validation is synchronous.
    assert.throws(() => db.incr("hits", 1.5), TypeError)
    assert.throws(() => db.incr("has space"), TypeError)
  })
})

test("topup fills to max and returns the overflow; take is all-or-nothing", async () => {
  await withClient({}, {}, async (db) => {
    await db.incr("quota", 98)
    assert.equal(await db.topup("quota", 5, 100), 3) // 98 → 100, 3 left over
    assert.equal(await db.incr("quota", 0), 100)
    assert.equal(await db.topup("quota", 5, 100), 5) // at max: nothing fits
    assert.equal(await db.topup("fresh", 5, 10), 0) // missing key counts as 0
    assert.equal(await db.incr("fresh", 0), 5)

    assert.equal(await db.take("quota", 100, 0), true) // drained to 0
    assert.equal(await db.take("quota", 1, 0), false) // a legit false result
    assert.equal(await db.incr("quota", 11), 11)
    assert.equal(await db.take("quota", 10, 0), true)
    assert.equal(await db.incr("quota", 0), 1)
    await assert.rejects(db.topup("quota", -1, 10), /non-negative/)
    assert.throws(() => db.topup("quota", 1.5, 10), TypeError)
  })
})

test("removeByPkLess/Greater return counts", async () => {
  await withClient({}, {}, async (db) => {
    for (const k of ["t:1", "t:2", "t:3", "t:4"]) await db.put(k, "x")
    assert.equal(await db.removeByPkLess("t:3"), 2) // t:1, t:2
    assert.equal(await db.removeByPkGreater("t:3"), 1) // t:4
    assert.deepEqual(await db.prefixScan("t:"), ["t:3"])
  })
})

test("stats reports store and server counters", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("k", { a: 1 })
    const s = await db.stats()
    assert.equal(s.records, 1)
    assert.ok(s.mainBytes > 0)
    assert.ok(s.connections >= 1)
    assert.ok(s.commands >= 1)
    for (const field of [
      "records",
      "mainBytes",
      "liveBytes",
      "blobBytes",
      "blobLiveBytes",
      "blobOrphanedBytes",
      "blobGenerations",
      "connections",
      "totalConns",
      "commands",
      "autoCompacts",
      "blobCompacts",
    ]) {
      assert.equal(typeof s[field], "number", field)
    }
  })
})

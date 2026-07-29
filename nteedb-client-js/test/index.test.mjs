import { test } from "node:test"
import assert from "node:assert/strict"
import { NteeServerError } from "../src/index.js"
import { withClient } from "./harness.mjs"

test("indexed put + secIndex/secIndexHas", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("call:1", { kind: "request" }, { traceId: "T1", status: 200 })
    await db.put("call:2", { kind: "request" }, { traceId: "T1", status: 500 })
    await db.put("call:3", { kind: "response" }, { traceId: "T2", status: 200 })

    assert.deepEqual(await db.secIndex("traceId", "T1"), ["call:1", "call:2"])
    assert.deepEqual(await db.secIndex("status", 200), ["call:1", "call:3"])
    assert.equal(await db.secIndexHas("traceId", "T2"), true)
    assert.equal(await db.secIndexHas("traceId", "T9"), false)
    assert.deepEqual(await db.secIndex("traceId", "T9"), [])
  })
})

test("jsonPath index derives values from the record", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("a", { kind: "request", n: 1 })
    await db.put("b", { kind: "response", n: 2 })
    assert.deepEqual(await db.secIndex("kind", "request"), ["a"])
    assert.deepEqual(await db.secIndex("kind", "response"), ["b"])
  })
})

test("secIndex limits: first N ascending, last N descending", async () => {
  await withClient({}, {}, async (db) => {
    for (let i = 1; i <= 5; i++) {
      await db.put(`k:${i}`, { n: i }, { traceId: "T" })
    }
    assert.deepEqual(await db.secIndex("traceId", "T", 2), ["k:1", "k:2"])
    assert.deepEqual(await db.secIndex("traceId", "T", -2), ["k:5", "k:4"])
    assert.equal((await db.secIndex("traceId", "T")).length, 5)
  })
})

test("secIndexPrefix with per-value limits", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("1", "{}", { traceId: "grp-a" })
    await db.put("2", "{}", { traceId: "grp-a" })
    await db.put("3", "{}", { traceId: "grp-b" })
    assert.deepEqual(await db.secIndexPrefix("traceId", "grp-"), [
      "1",
      "2",
      "3",
    ])
    // limit -1: the last key of each distinct value, descending per group.
    assert.deepEqual(await db.secIndexPrefix("traceId", "grp-", -1), ["2", "3"])
    assert.deepEqual(await db.secIndexPrefix("traceId", "zzz"), [])
  })
})

test("secIndexRange is inclusive at both ends", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("s1", "{}", { status: 100 })
    await db.put("s2", "{}", { status: 200 })
    await db.put("s3", "{}", { status: 300 })
    assert.deepEqual(await db.secIndexRange("status", 100, 300), [
      "s1",
      "s2",
      "s3",
    ])
    assert.deepEqual(await db.secIndexRange("status", 101, 299), ["s2"])
  })
})

test("secIndexRecords returns decoded {key, value} rows", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("r1", { n: 1 }, { traceId: "T" })
    await db.put("r2", { n: 2 }, { traceId: "T" })
    assert.deepEqual(await db.secIndexRecords("traceId", "T"), [
      { key: "r1", value: { n: 1 } },
      { key: "r2", value: { n: 2 } },
    ])
  })
})

test("index errors and client-side validation", async () => {
  await withClient({}, {}, async (db) => {
    await assert.rejects(db.secIndex("ghost", "x"), NteeServerError)
    await assert.rejects(db.secIndex("ghost", "x"), /unknown index/)
    // Kind mismatch surfaces the server's coercion error.
    await assert.rejects(db.secIndex("status", "abc"), /is not a number/)
    // putx with a non-object value is rejected by the server, nothing written.
    await assert.rejects(
      db.put("imm", "just-a-string", { traceId: "T" }),
      /immediate value/,
    )
    assert.equal(await db.has("imm"), false)
    // Client-side: ix must be a plain object; string index values must be
    // whitespace-free tokens.
    assert.throws(() => db.put("k", "{}", ["not", "an", "object"]), TypeError)
    assert.throws(() => db.secIndex("traceId", "has space"), TypeError)
  })
})

test("dropped/prospective are empty on a clean schema", async () => {
  await withClient({}, {}, async (db) => {
    assert.deepEqual(await db.secIndexDropped(), [])
    assert.deepEqual(await db.secIndexProspective(), [])
  })
})

import { test } from "node:test"
import assert from "node:assert/strict"
import { mkdtemp, rm } from "node:fs/promises"
import { tmpdir } from "node:os"
import path from "node:path"
import { NteeDB } from "../src/index.js"

async function withDB(opts, fn) {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  const db = NteeDB.open(dir, opts)
  try {
    await fn(db, dir)
  } finally {
    try {
      db.close()
    } catch {}
    await rm(dir, { recursive: true, force: true })
  }
}

test("single-writer lock: second open of the same store throws", async () => {
  await withDB({}, async (db, dir) => {
    // The store is held by `db` — a second opener must fail fast.
    assert.throws(() => NteeDB.open(dir, {}), /locked/)
    // The first handle is unaffected.
    db.put("k", "v")
    assert.equal((await db.get("k")).toString(), "v")
    // After releasing, the store opens again.
    db.close()
    const db2 = NteeDB.open(dir, {})
    assert.equal((await db2.get("k")).toString(), "v")
    db2.close()
  })
})

test("put/get/has/delete round-trip", async () => {
  await withDB({}, async (db) => {
    db.put("alpha", Buffer.from("one"))
    db.put("beta", "two") // string accepted
    assert.equal((await db.get("alpha")).toString(), "one")
    assert.equal((await db.get("beta")).toString(), "two")
    assert.equal(await db.has("beta"), true)
    assert.equal(await db.get("missing"), null)

    db.put("alpha", Buffer.from("ONE"))
    assert.equal((await db.get("alpha")).toString(), "ONE")

    db.delete("alpha")
    assert.equal(await db.get("alpha"), null)
    assert.equal(await db.has("alpha"), false)
  })
})

test("putMany batches records (order, indexes, validation, caps)", async () => {
  await withDB(
    { indexes: [{ name: "traceId", kind: "string", maxPerValue: 2 }] },
    async (db) => {
      // Indexed values must be JSON objects (immediate values are PK-only).
      const n = await db.putMany([
        { key: "call:1", value: { v: "a" }, ix: { traceId: "T" } },
        { key: "call:2", value: { v: "b" }, ix: { traceId: "T" } },
        { key: "call:3", value: { v: "c" }, ix: { traceId: "T" } },
        { key: "other:1", value: "x" },
        { key: "other:1", value: "y" }, // later item for the same key wins
      ])
      assert.equal(n, 5)

      // Values round-trip; last write in the batch wins.
      assert.equal((await db.get("other:1")).toString(), "y")
      assert.deepEqual(await db.get("call:2"), { v: "b" })

      // maxPerValue applied across the batch: only the newest 2 remain.
      assert.deepEqual(await db.secIndex("traceId", "T"), ["call:2", "call:3"])
      assert.equal(await db.has("call:1"), false)

      // An invalid item rejects the whole batch with nothing written.
      await assert.rejects(
        db.putMany([
          { key: "ok:1", value: "v" },
          { key: "bad:1", value: "{}", ix: { nope: "x" } },
        ]),
        /unknown index/,
      )
      assert.equal(await db.has("ok:1"), false)

      // An immediate (non-object) value cannot carry index values at all.
      await assert.rejects(
        db.putMany([{ key: "bad:2", value: "scalar", ix: { traceId: "T" } }]),
        /immediate value/,
      )
      assert.equal(await db.has("bad:2"), false)

      // A JSON value reads back parsed; binary and a BOM-prefixed value are
      // non-JSON → byte-exact Buffers.
      const binary = Buffer.from([0xff, 0xfe, 0x00, 0x01, 0xc3, 0x28])
      const bom = Buffer.from([0xef, 0xbb, 0xbf, 0x68, 0x69]) // BOM + "hi"
      await db.putMany([
        { key: "batch:text", value: '{"n":1,"emoji":"🎉"}' },
        { key: "batch:bin", value: binary },
        { key: "batch:bom", value: bom },
      ])
      assert.deepEqual(await db.get("batch:text"), { n: 1, emoji: "🎉" })
      assert.deepEqual(await db.get("batch:bin"), binary)
      assert.deepEqual(await db.get("batch:bom"), bom)
    },
  )
})

test("prefix scan", async () => {
  await withDB({}, async (db) => {
    for (const k of [
      "input:Get",
      "input:GetProperty",
      "input:GetPropertyNames",
      "api:/x",
      "input:SetX",
    ]) {
      db.put(k, "v")
    }
    assert.deepEqual(await db.prefixScan("input:GetP"), [
      "input:GetProperty",
      "input:GetPropertyNames",
    ])
    assert.equal((await db.prefixScan("input:")).length, 4)
  })
})

test("JSON values parse; invalid-UTF-8 binary round-trips as a Buffer", async () => {
  await withDB({}, async (db) => {
    // A JSON value is stored readably on disk ("s") and read back parsed.
    const obj = { endpoint: "/api/users", note: "emoji 🎉" }
    db.put("obj", JSON.stringify(obj))
    assert.deepEqual(await db.get("obj"), obj)

    // Binary (invalid UTF-8) is base64 on the wire → Buffer, byte-exact.
    const binary = Buffer.from([0xff, 0xfe, 0x00, 0x01, 0xc3, 0x28])
    db.put("bin", binary)
    assert.deepEqual(await db.get("bin"), binary)

    // Empty value is not valid JSON → an empty Buffer.
    db.put("empty", "")
    assert.deepEqual(await db.get("empty"), Buffer.alloc(0))
  })
})

test("binary values survive (blob path)", async () => {
  await withDB({ blobThreshold: 32 }, async (db) => {
    const big = Buffer.alloc(4096, 0xab)
    db.put("blob", big)
    assert.ok((await db.get("blob")).equals(big))
  })
})

test("getMany: order preserved, missing → null, text/binary/blob values", async () => {
  await withDB({ blobThreshold: 32 }, async (db) => {
    const binary = Buffer.from([0xff, 0xfe, 0x00, 0x01])
    const big = Buffer.alloc(4096, 0xcd) // over blobThreshold → blob path
    db.put("text", "hello")
    db.put("bin", binary)
    db.put("blob", big)

    const got = await db.getMany(["blob", "missing", "text", "bin"])
    assert.equal(got.length, 4)
    assert.ok(got[0].equals(big)) // aligned to input order
    assert.equal(got[1], null) // absent key → null
    assert.equal(got[2].toString(), "hello")
    assert.deepEqual(got[3], binary)

    assert.deepEqual(await db.getMany([]), []) // empty input
  })
})

test("put/putMany accept an object directly (JSON-serialized)", async () => {
  await withDB({}, async (db) => {
    const obj = { kind: "request", tags: ["a", "b"] }
    db.put("a", obj) // object, not a string
    assert.deepEqual(await db.get("a"), obj)

    db.put("n", 42) // scalar
    assert.equal(await db.get("n"), 42)

    // string and Buffer still pass through unchanged (no double-encoding).
    db.put("s", '{"pre":"serialized"}')
    assert.deepEqual(await db.get("s"), { pre: "serialized" })

    await db.putMany([
      { key: "b1", value: { n: 1 } },
      { key: "b2", value: "raw-string" }, // stays a string → non-JSON → Buffer
    ])
    assert.deepEqual(await db.get("b1"), { n: 1 })
    assert.ok(Buffer.isBuffer(await db.get("b2")))
    assert.equal((await db.get("b2")).toString(), "raw-string")
  })
})

test("putMany binary ABI: empty batch, blob-path + mixed values, byte-exact", async () => {
  await withDB({ blobThreshold: 64 }, async (db) => {
    assert.equal(await db.putMany([]), 0) // empty batch is a no-op

    const big = Buffer.alloc(4096, 0x5a) // over blobThreshold → blob path
    const bin = Buffer.from([0xff, 0x00, 0xfe])
    const n = await db.putMany([
      { key: "k:big", value: big },
      { key: "k:obj", value: { a: 1 } },
      { key: "k:bin", value: bin },
      { key: "k:str", value: "plain" }, // non-JSON string → Buffer on read
    ])
    assert.equal(n, 4)
    assert.ok((await db.get("k:big")).equals(big))
    assert.deepEqual(await db.get("k:obj"), { a: 1 })
    assert.deepEqual(await db.get("k:bin"), bin)
    assert.equal((await db.get("k:str")).toString(), "plain")
  })
})

test("JSON store: reads return parsed values, Buffer for non-JSON", async () => {
  await withDB(
    { indexes: [{ name: "traceId", kind: "string" }] },
    async (db) => {
      const obj = { endpoint: "/api/users", status: 200, tags: ["a", "b"] }
      db.put("rec", JSON.stringify(obj), { traceId: "T1" })
      db.put("rec2", JSON.stringify({ n: 2 }), { traceId: "T1" })
      db.put("text", "hello") // not valid JSON → Buffer fallback
      db.put("bin", Buffer.from([0xff, 0x00, 0x01])) // binary → Buffer

      // get: parsed value, not a Buffer.
      assert.deepEqual(await db.get("rec"), obj)
      assert.equal(await db.get("missing"), null)
      // Non-JSON and binary fall back to Buffer.
      assert.ok(Buffer.isBuffer(await db.get("text")))
      assert.equal((await db.get("text")).toString(), "hello")
      assert.deepEqual(await db.get("bin"), Buffer.from([0xff, 0x00, 0x01]))

      // getMany + secIndexRecords return parsed values too.
      assert.deepEqual(await db.getMany(["rec", "missing"]), [obj, null])
      const recs = await db.secIndexRecords("traceId", "T1")
      assert.deepEqual(
        recs.map((r) => r.value),
        [obj, { n: 2 }],
      )

      // Documented JSON parse semantics: a stored scalar coerces.
      db.put("num", "123")
      assert.equal(await db.get("num"), 123)
    },
  )
})

test("secondary indexes: explicit values, multi-value, range, prefix", async () => {
  await withDB(
    {
      indexes: [
        { name: "traceId", kind: "string" },
        { name: "status", kind: "number" },
      ],
    },
    async (db) => {
      db.put("call:1", "{}", { traceId: "T1", status: 200 })
      db.put("call:2", "{}", { traceId: "T1", status: 404 })
      db.put("call:3", "{}", { traceId: "T2", status: 200 })

      assert.deepEqual(await db.secIndex("traceId", "T1"), ["call:1", "call:2"])
      assert.deepEqual(await db.secIndexRange("status", 200, 299), [
        "call:1",
        "call:3",
      ])
      assert.deepEqual(await db.secIndexPrefix("traceId", "T"), [
        "call:1",
        "call:2",
        "call:3",
      ])

      // secIndexHas: presence per value, both kinds; false after the value
      // has no more records.
      assert.equal(await db.secIndexHas("traceId", "T1"), true)
      assert.equal(await db.secIndexHas("traceId", "T9"), false)
      assert.equal(await db.secIndexHas("status", 404), true)
      assert.equal(await db.secIndexHas("status", 500), false)
      await assert.rejects(db.secIndexHas("nope", "x"), /unknown index/)

      db.delete("call:1")
      assert.deepEqual(await db.secIndex("traceId", "T1"), ["call:2"])
      db.delete("call:2")
      assert.equal(await db.secIndexHas("traceId", "T1"), false) // both gone
    },
  )
})

test("secIndexPrefix grouped +/-N limit + secIndexPrefixRecords records", async () => {
  await withDB(
    { indexes: [{ name: "endpoint", kind: "string" }] },
    async (db) => {
      // Two records under GetXXXMutation, one under GetXXXMumu. Sorted by
      // (value, pk): GetXXXMumu < GetXXXMutation ('m' < 't').
      db.put("call:1", { v: "a" }, { endpoint: "GetXXXMutation" })
      db.put("call:2", { v: "b" }, { endpoint: "GetXXXMutation" })
      db.put("call:3", { v: "c" }, { endpoint: "GetXXXMumu" })

      // limit 0 (default) is unchanged: all matches, flat, in (value, pk) order.
      assert.deepEqual(await db.secIndexPrefix("endpoint", "GetXXXM"), [
        "call:3",
        "call:1",
        "call:2",
      ])
      // -1: last record of each endpoint (groups ascending by value).
      assert.deepEqual(await db.secIndexPrefix("endpoint", "GetXXXM", -1), [
        "call:3",
        "call:2",
      ])
      // +1: first record of each endpoint.
      assert.deepEqual(await db.secIndexPrefix("endpoint", "GetXXXM", 1), [
        "call:3",
        "call:1",
      ])

      // secIndexPrefixRecords returns full records in the same order.
      const recs = await db.secIndexPrefixRecords("endpoint", "GetXXXM", -1)
      assert.deepEqual(
        recs.map((r) => r.key),
        ["call:3", "call:2"],
      )
      assert.deepEqual(recs[0].value, { v: "c" })
      assert.deepEqual(recs[1].value, { v: "b" })
    },
  )
})

test("removeByPkLess / removeByPkGreater (range delete, count, secondary sweep)", async () => {
  await withDB(
    { indexes: [{ name: "traceId", kind: "string" }] },
    async (db) => {
      for (let i = 1; i <= 5; i++)
        db.put(`call:${i}`, "{}", { traceId: `T${i}` })

      // Strict: call:3 (the cutoff) survives; call:1/2 removed. Resolves to count.
      assert.equal(await db.removeByPkLess("call:3"), 2)
      assert.equal(await db.get("call:1"), null)
      assert.equal(await db.has("call:3"), true)
      // Secondary swept — no ghost for a deleted key.
      assert.deepEqual(await db.secIndex("traceId", "T1"), [])
      assert.deepEqual(await db.secIndex("traceId", "T3"), ["call:3"])

      // Strict greater: call:4/5 removed, call:3 kept.
      assert.equal(await db.removeByPkGreater("call:3"), 2)
      assert.equal(await db.has("call:4"), false)
      assert.equal(await db.has("call:3"), true)
      assert.deepEqual(await db.secIndex("traceId", "T5"), [])

      // No-op range removes nothing.
      assert.equal(await db.removeByPkLess("call:3"), 0)
    },
  )
})

test("maxPerValue caps records per index value (oldest evicted)", async () => {
  await withDB(
    { indexes: [{ name: "traceId", kind: "string", maxPerValue: 2 }] },
    async (db) => {
      db.put("call:1", { v: "a" }, { traceId: "T" })
      db.put("call:2", { v: "b" }, { traceId: "T" })
      // Third record with the same value: lowest pk (call:1) is fully deleted.
      db.put("call:3", { v: "c" }, { traceId: "T" })

      assert.equal(await db.get("call:1"), null)
      assert.equal(await db.has("call:1"), false)
      assert.deepEqual(await db.secIndex("traceId", "T"), ["call:2", "call:3"])

      // A different value has its own budget.
      db.put("other:1", { v: "x" }, { traceId: "U" })
      assert.deepEqual(await db.secIndex("traceId", "U"), ["other:1"])
      assert.deepEqual(await db.secIndex("traceId", "T"), ["call:2", "call:3"])
    },
  )
})

test("secIndex limit + direction (first N asc / last N desc)", async () => {
  await withDB(
    { indexes: [{ name: "traceId", kind: "string" }] },
    async (db) => {
      for (let i = 1; i <= 6; i++) db.put(`call:${i}`, "{}", { traceId: "T" })
      assert.deepEqual(await db.secIndex("traceId", "T"), [
        "call:1",
        "call:2",
        "call:3",
        "call:4",
        "call:5",
        "call:6",
      ])
      assert.deepEqual(await db.secIndex("traceId", "T", 3), [
        "call:1",
        "call:2",
        "call:3",
      ]) // first 3 asc
      assert.deepEqual(await db.secIndex("traceId", "T", -2), [
        "call:6",
        "call:5",
      ]) // last 2 desc
      assert.deepEqual(await db.secIndex("traceId", "T", 100), [
        "call:1",
        "call:2",
        "call:3",
        "call:4",
        "call:5",
        "call:6",
      ]) // clamps
      assert.deepEqual(await db.secIndex("traceId", "missing", -5), [])
      const recent = await db.secIndexRecords("traceId", "T", -2)
      assert.deepEqual(
        recent.map((r) => r.key),
        ["call:6", "call:5"],
      )
    },
  )
})

test("jsonPath extractor + secIndexRecords returns records", async () => {
  await withDB(
    { indexes: [{ name: "kind", kind: "string", jsonPath: "kind" }] },
    async (db) => {
      db.put("r1", JSON.stringify({ kind: "request", n: 1 }))
      db.put("r2", JSON.stringify({ kind: "history", n: 2 }))
      db.put("r3", JSON.stringify({ kind: "request", n: 3 }))

      const recs = await db.secIndexRecords("kind", "request")
      assert.deepEqual(
        recs.map((r) => r.key),
        ["r1", "r3"],
      )
      assert.equal(recs[0].value.n, 1) // value is the parsed record
    },
  )
})

test("reopen restores state", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    const db = NteeDB.open(dir, {})
    db.put("a", "1")
    db.put("b", "2")
    db.delete("b")
    db.close()

    const db2 = NteeDB.open(dir, {})
    assert.equal((await db2.get("a")).toString(), "1")
    assert.equal(await db2.get("b"), null)
    db2.close()
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("async compact and reindex", async () => {
  await withDB(
    { indexes: [{ name: "kind", kind: "string", jsonPath: "kind" }] },
    async (db) => {
      db.put("r1", JSON.stringify({ kind: "a" }))
      db.put("r1", JSON.stringify({ kind: "a" })) // dead record
      db.put("r2", JSON.stringify({ kind: "b" }))
      await db.compact()
      assert.deepEqual(await db.secIndex("kind", "a"), ["r1"])
      await db.reindex()
      assert.deepEqual(await db.secIndex("kind", "b"), ["r2"])
    },
  )
})

test("error surfaces as thrown Error", async () => {
  await withDB(
    { indexes: [{ name: "status", kind: "number" }] },
    async (db) => {
      assert.throws(
        () => db.put("k", "{}", { unknownIndex: "x" }),
        /unknown index/,
      )
      // Explicit index values on an immediate (non-object) value are rejected.
      assert.throws(
        () => db.put("k", "scalar", { status: 200 }),
        /immediate value/,
      )
      await assert.rejects(db.secIndex("nope", "x"), /unknown index/)
    },
  )
})

test("incr/decr: atomic int64 counters", async () => {
  await withDB({}, async (db) => {
    // Missing key initializes to 0; delta defaults to 1.
    assert.equal(await db.incr("hits"), 1)
    assert.equal(await db.incr("hits", 41), 42)
    assert.equal(await db.decr("hits", 2), 40)
    assert.equal(await db.decr("hits"), 39)
    // Read idiom: delta 0 returns the current value.
    assert.equal(await db.incr("hits", 0), 39)
    // Negative init and sign crossing.
    assert.equal(await db.incr("neg", -100), -100)
    assert.equal(await db.incr("neg", 150), 50)

    // Counters are a distinct type: incr on a plain value rejects, untouched.
    db.put("plain", "hello")
    await assert.rejects(db.incr("plain"), /non-counter/)
    assert.equal((await db.get("plain")).toString(), "hello")

    // Deltas must be safe integers — rejected synchronously.
    assert.throws(() => db.incr("hits", 1.5), TypeError)
    assert.throws(() => db.decr("hits", NaN), TypeError)
  })
})

test("counters persist across close/reopen", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    let db = NteeDB.open(dir, {})
    assert.equal(await db.incr("c", 7), 7)
    assert.equal(await db.incr("c", 7), 14)
    db.close()

    db = NteeDB.open(dir, {})
    assert.equal(await db.incr("c", 0), 14)
    db.close()
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("drop deletes the store", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    const db = NteeDB.open(dir, {})
    db.put("a", "1")
    db.drop()
    const db2 = NteeDB.open(dir, {})
    assert.equal(await db2.has("a"), false)
    db2.close()
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("stats reports records and file sizes", async () => {
  await withDB({ blobThreshold: 32 }, async (db) => {
    const empty = await db.stats()
    assert.deepEqual(empty, { records: 0, mainBytes: 0, blobBytes: 0 })

    db.put("a", { n: 1 })
    db.put("b", Buffer.alloc(100, 0xcd)) // blob path
    const s = await db.stats()
    assert.equal(s.records, 2)
    assert.ok(s.mainBytes > 0)
    assert.equal(s.blobBytes, 100)

    db.delete("a")
    const s2 = await db.stats()
    assert.equal(s2.records, 1)
    assert.ok(s2.mainBytes > s.mainBytes) // tombstone appended until compact
  })
})

test("static destroy deletes a closed store's files", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    const db = NteeDB.open(dir, {})
    db.put("a", { n: 1 })
    db.close()

    NteeDB.destroy(dir)
    const db2 = NteeDB.open(dir, {}) // fresh store: nothing survives
    assert.equal(await db2.get("a"), null)
    db2.close()
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("secIndexDropped / secIndexProspective report schema state", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    // Declare an index, write through it, close.
    const db = NteeDB.open(dir, { indexes: [{ name: "tmp", kind: "string" }] })
    db.put("k1", { v: 1 }, { tmp: "T" })
    db.close()

    // Reopen without it → soft-dropped; with a NEW index over existing data →
    // prospective (covers only future writes until reindex()).
    const db2 = NteeDB.open(dir, {
      indexes: [{ name: "later", kind: "string", jsonPath: "v" }],
    })
    assert.deepEqual(await db2.secIndexDropped(), ["tmp"])
    assert.deepEqual(await db2.secIndexProspective(), ["later"])

    // reindex back-fills the jsonPath index and purges the dropped one.
    await db2.reindex()
    assert.deepEqual(await db2.secIndexDropped(), [])
    assert.deepEqual(await db2.secIndexProspective(), [])
    db2.close()
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("operations on a closed handle throw", async () => {
  const dir = await mkdtemp(path.join(tmpdir(), "nteedb-"))
  try {
    const db = NteeDB.open(dir, {})
    db.put("a", { n: 1 })
    db.close()
    db.close() // idempotent

    for (const op of [
      () => db.get("a"),
      () => db.put("b", { n: 2 }),
      () => db.incr("c"),
      () => db.decr("c"),
      () => db.stats(),
      () => db.prefixScan(""),
      () => db.drop(),
    ]) {
      assert.throws(op, /database is closed/)
    }
  } finally {
    await rm(dir, { recursive: true, force: true })
  }
})

test("put(key, undefined) throws a clear error", async () => {
  await withDB({}, async (db) => {
    assert.throws(() => db.put("k", undefined), /value is undefined/)
    // putMany validates the payload before going async → throws synchronously,
    // like its closed-handle guard.
    assert.throws(
      () => db.putMany([{ key: "k", value: undefined }]),
      /value is undefined/,
    )
    assert.equal(await db.has("k"), false)
  })
})

test("overlapping async operations settle consistently", async () => {
  await withDB({}, async (db) => {
    const items = Array.from({ length: 500 }, (_, i) => ({
      key: "k" + String(i).padStart(4, "0"),
      value: { i },
    }))
    // Fire batch writes, compaction, and a range delete concurrently — all run
    // on libuv worker threads against the same handle.
    const [n] = await Promise.all([
      db.putMany(items),
      db.compact(),
      db.reindex(),
    ])
    assert.equal(n, 500)
    await db.removeByPkLess("k0100")
    assert.equal(await db.get("k0099"), null)
    assert.deepEqual(await db.get("k0100"), { i: 100 })
    assert.equal((await db.stats()).records, 400)
  })
})

test("no memory leak across many calls (RSS stays bounded)", async () => {
  await withDB({}, async (db) => {
    const big = Buffer.alloc(64 * 1024, 0x7a) // 64 KiB
    for (let i = 0; i < 50; i++) db.put("k" + i, big)
    if (global.gc) global.gc()
    const before = process.memoryUsage().rss
    for (let i = 0; i < 20000; i++) {
      const v = await db.get("k" + (i % 50))
      assert.equal(v.length, big.length)
    }
    if (global.gc) global.gc()
    const after = process.memoryUsage().rss
    // 20k gets of a 64 KiB value would balloon RSS if the C buffers leaked.
    const growthMB = (after - before) / (1024 * 1024)
    assert.ok(
      growthMB < 64,
      `RSS grew ${growthMB.toFixed(1)} MB across 20k gets (possible leak)`,
    )
  })
})

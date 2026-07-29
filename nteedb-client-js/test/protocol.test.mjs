import { test } from "node:test"
import assert from "node:assert/strict"
import {
  toStorable,
  assertToken,
  decodeValue,
  buildPut,
  buildPutx,
} from "../src/protocol.js"
import { NteeClient, NteeConnectionError } from "../src/index.js"
import { withClient, withServer } from "./harness.mjs"

// ---- pure unit tests (no server) ----

test("toStorable: strings and Buffers pass through, objects stringify", () => {
  assert.equal(toStorable("hi"), "hi")
  const buf = Buffer.from([1, 2, 3])
  assert.equal(toStorable(buf), buf)
  assert.equal(toStorable({ a: 1 }), '{"a":1}')
  assert.equal(toStorable([1, 2]), "[1,2]")
  assert.equal(toStorable(42), "42")
  assert.equal(toStorable(null), "null")
  assert.throws(() => toStorable(undefined), /value is undefined/)
})

test("assertToken rejects whitespace and non-strings", () => {
  assertToken("key", "a-ok:123")
  for (const bad of ["", "a b", "a\tb", "a\nb", "a\rb", " a"]) {
    assert.throws(() => assertToken("key", bad), TypeError)
  }
  assert.throws(() => assertToken("key", 42), TypeError)
  assert.throws(() => assertToken("key", null), TypeError)
})

test("decodeValue: exact bin envelope becomes a Buffer, everything else passes", () => {
  const decoded = decodeValue({ bin: true, base64: "aGk=" })
  assert.ok(Buffer.isBuffer(decoded))
  assert.equal(decoded.toString(), "hi")
  // Not the exact shape: pass through untouched.
  for (const v of [
    { bin: true, base64: "aGk=", extra: 1 },
    { bin: false, base64: "aGk=" },
    { bin: true },
    { a: 1 },
    [1, 2],
    "str",
    42,
    null,
    true,
  ]) {
    assert.deepEqual(decodeValue(v), v)
  }
})

test("buildPut produces the exact length-prefixed frame", () => {
  assert.deepEqual(
    buildPut("k1", Buffer.from("hello")),
    Buffer.from("put k1 5\r\nhello\r\n"),
  )
  // Empty value and multibyte lengths are byte counts, not char counts.
  assert.deepEqual(
    buildPut("k", Buffer.alloc(0)),
    Buffer.from("put k 0\r\n\r\n"),
  )
  const snowman = Buffer.from("☃") // 3 bytes UTF-8
  assert.deepEqual(
    buildPut("k", snowman),
    Buffer.concat([Buffer.from("put k 3\r\n"), snowman, Buffer.from("\r\n")]),
  )
})

test("buildPutx produces the two-block frame", () => {
  const ix = Buffer.from('{"traceId":"T1"}')
  const val = Buffer.from('{"a":1}')
  assert.deepEqual(
    buildPutx("call:1", ix, val),
    Buffer.concat([
      Buffer.from(`putx call:1 ${ix.length} ${val.length}\r\n`),
      ix,
      Buffer.from("\r\n"),
      val,
      Buffer.from("\r\n"),
    ]),
  )
})

// ---- server-backed wire tests ----

test("pipelining: many unawaited commands resolve in order", async () => {
  await withClient({}, {}, async (db) => {
    const puts = []
    for (let i = 0; i < 20; i++) puts.push(db.put(`k:${i}`, { i }))
    const incrs = []
    for (let i = 0; i < 20; i++) incrs.push(db.incr("c"))
    await Promise.all(puts)
    assert.deepEqual(
      await Promise.all(incrs),
      [...Array(20)].map((_, i) => i + 1),
    )
    const gets = await Promise.all(
      [...Array(20)].map((_, i) => db.get(`k:${i}`)),
    )
    gets.forEach((v, i) => assert.deepEqual(v, { i }))
  })
})

test("binary round-trip: all byte values survive", async () => {
  await withClient({}, {}, async (db) => {
    const bytes = Buffer.from([...Array(256)].map((_, i) => i))
    await db.put("bin", bytes)
    const back = await db.get("bin")
    assert.ok(Buffer.isBuffer(back))
    assert.deepEqual(back, bytes)
  })
})

test("value encoding quirks: text becomes Buffer, JSON text becomes value", async () => {
  await withClient({}, {}, async (db) => {
    // Plain text is not valid JSON → comes back as a Buffer.
    await db.put("txt", "hello world")
    const txt = await db.get("txt")
    assert.ok(Buffer.isBuffer(txt))
    assert.equal(txt.toString(), "hello world")
    // The string "123" IS valid JSON → comes back as the number 123.
    await db.put("num", "123")
    assert.equal(await db.get("num"), 123)
    // A stored literal bin-envelope object decodes to a Buffer (documented
    // wire-level ambiguity).
    await db.put("amb", { bin: true, base64: "AAE=" })
    const amb = await db.get("amb")
    assert.ok(Buffer.isBuffer(amb))
    assert.deepEqual(amb, Buffer.from([0, 1]))
    // Empty values and embedded CRLF round-trip via the framed put.
    await db.put("empty", "")
    assert.deepEqual(await db.get("empty"), Buffer.alloc(0))
    await db.put("crlf", "a\r\nb")
    assert.equal((await db.get("crlf")).toString(), "a\r\nb")
  })
})

test("large value round-trip (2 MiB, above the 1 MiB line limit)", async () => {
  await withClient({}, {}, async (db) => {
    const big = Buffer.alloc(2 * 1024 * 1024, 0xab)
    await db.put("big", big)
    const back = await db.get("big")
    assert.deepEqual(back, big)
  })
})

test("command(): raw envelope, no throw on ok:false", async () => {
  await withClient({}, {}, async (db) => {
    await db.put("k", { a: 1 })
    const scan = await db.command("scan")
    assert.equal(scan.ok, true)
    assert.deepEqual(scan.result, ["k"])
    const bogus = await db.command("frobnicate now")
    assert.equal(bogus.ok, false)
    assert.match(bogus.err, /unknown command/)
    assert.throws(() => db.command(""), TypeError)
    assert.throws(() => db.command("get\nk"), TypeError)
  })
})

test("protocol-fatal error closes the connection and rejects everything", async () => {
  await withServer({}, async (srv) => {
    const db = await NteeClient.connect({ host: srv.host, port: srv.port })
    // A command line above MaxLine (1 MiB) is protocol-fatal: one error
    // response, then the server closes the socket.
    const huge = db.get("x".repeat(2 * 1024 * 1024))
    const queued = db.get("y".repeat(2 * 1024 * 1024))
    await assert.rejects(huge, /line too long|connection/)
    await assert.rejects(queued)
    // The connection is dead: later calls reject with NteeConnectionError.
    await assert.rejects(db.ping(), NteeConnectionError)
    assert.equal(db.closed, true)
  })
})

test("quit/close are graceful and idempotent", async () => {
  await withServer({}, async (srv) => {
    const db = await NteeClient.connect({ host: srv.host, port: srv.port })
    await db.put("k", "v")
    await db.close()
    assert.equal(db.closed, true)
    await db.close() // no-op
    await assert.rejects(db.get("k"), NteeConnectionError)
  })
})

test("server death rejects in-flight and later commands", async () => {
  await withServer({}, async (srv) => {
    const db = await NteeClient.connect({ host: srv.host, port: srv.port })
    await db.put("k", "v")
    srv.child.kill("SIGKILL")
    await new Promise((resolve) => srv.child.once("exit", resolve))
    // In-flight or post-mortem commands fail with a connection error.
    await assert.rejects(
      Promise.all([db.get("k"), db.ping()]),
      NteeConnectionError,
    )
  })
})

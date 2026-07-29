import { test } from "node:test"
import assert from "node:assert/strict"
import { readFile } from "node:fs/promises"
import { NteeClient, NteeConnectionError } from "../src/index.js"
import { withServer, TLS_CERT } from "./harness.mjs"

const ca = await readFile(TLS_CERT)

test("full command round-trip over TLS", async () => {
  await withServer({ tls: true }, async (srv) => {
    const db = await NteeClient.connect({
      host: srv.host,
      port: srv.tlsPort,
      tls: { ca },
    })
    try {
      assert.equal(db.info.server, "nteedb")
      await db.put("k", { a: 1 }, { traceId: "T1" })
      assert.deepEqual(await db.get("k"), { a: 1 })
      assert.deepEqual(await db.secIndex("traceId", "T1"), ["k"])
      assert.equal(await db.incr("c", 5), 5)
      assert.equal(await db.topup("c", 5, 8), 2)
      assert.equal(await db.take("c", 8, 0), true)
      // Binary survives the encrypted transport too.
      const bytes = Buffer.from([...Array(256)].map((_, i) => i))
      await db.put("bin", bytes)
      assert.deepEqual(await db.get("bin"), bytes)
    } finally {
      await db.close()
    }
  })
})

test("plain and TLS clients share the same store", async () => {
  await withServer({ tls: true }, async (srv) => {
    const plain = await NteeClient.connect({ host: srv.host, port: srv.port })
    const secure = await NteeClient.connect({
      host: srv.host,
      port: srv.tlsPort,
      tls: { ca },
    })
    try {
      await plain.put("via-plain", { n: 1 })
      assert.deepEqual(await secure.get("via-plain"), { n: 1 })
      await secure.put("via-tls", { n: 2 })
      assert.deepEqual(await plain.get("via-tls"), { n: 2 })
    } finally {
      await plain.close()
      await secure.close()
    }
  })
})

test("self-signed cert is rejected without the ca (and accepted insecurely)", async () => {
  await withServer({ tls: true }, async (srv) => {
    // Default verification (system CAs) must refuse the self-signed cert.
    await assert.rejects(
      NteeClient.connect({
        host: srv.host,
        port: srv.tlsPort,
        tls: true,
        connectTimeout: 3000,
      }),
      NteeConnectionError,
    )
    // Explicitly disabling verification connects (not recommended — docs).
    const db = await NteeClient.connect({
      host: srv.host,
      port: srv.tlsPort,
      tls: { rejectUnauthorized: false },
    })
    try {
      assert.equal(await db.ping(), "pong")
    } finally {
      await db.close()
    }
  })
})

test("auth works over TLS", async () => {
  await withServer({ tls: true, auth: "s3cret" }, async (srv) => {
    const db = await NteeClient.connect({
      host: srv.host,
      port: srv.tlsPort,
      tls: { ca },
      auth: "s3cret",
    })
    try {
      assert.ok(Array.isArray(db.info.indexes))
      await db.put("k", "v")
      assert.equal(await db.has("k"), true)
    } finally {
      await db.close()
    }
  })
})

test("connecting plainly to the TLS port fails cleanly", async () => {
  await withServer({ tls: true }, async (srv) => {
    // The TLS listener drops a non-TLS client at the handshake; the connect
    // (which includes the hello round-trip) must fail, not hang.
    await assert.rejects(
      NteeClient.connect({
        host: srv.host,
        port: srv.tlsPort,
        connectTimeout: 3000,
      }),
      NteeConnectionError,
    )
  })
})

import { test } from "node:test"
import assert from "node:assert/strict"
import { NteeClient, NteeServerError } from "../src/index.js"
import { withServer, withClient } from "./harness.mjs"

test("password mode: pre-auth hello works, data commands require auth", async () => {
  await withServer({ auth: "s3cret" }, async (srv) => {
    const db = await NteeClient.connect({ host: srv.host, port: srv.port })
    try {
      assert.equal(db.info.auth, "password")
      assert.equal(db.info.indexes, undefined) // schema hidden pre-auth
      await assert.rejects(db.get("k"), /auth required/)
      await assert.rejects(db.ping(), /auth required/)

      assert.equal(await db.auth("s3cret"), true)
      assert.ok(Array.isArray(db.info.indexes)) // hello re-ran post-auth
      await db.put("k", { a: 1 })
      assert.deepEqual(await db.get("k"), { a: 1 })
      // Password mode grants admin.
      assert.equal(await db.compact(), true)
    } finally {
      await db.close()
    }
  })
})

test("password mode: connect({auth}) authenticates in one step", async () => {
  await withClient({ auth: "s3cret" }, { auth: "s3cret" }, async (db) => {
    assert.ok(Array.isArray(db.info.indexes))
    await db.put("k", "v")
    assert.equal(await db.has("k"), true)
  })
})

test("password mode: wrong password rejects connect", async () => {
  await withServer({ auth: "s3cret" }, async (srv) => {
    await assert.rejects(
      NteeClient.connect({ host: srv.host, port: srv.port, auth: "wrong" }),
      /invalid password/,
    )
  })
})

const USERS_FILE = [
  "# test users",
  "root:changeme:admin",
  "bob:hunter2:user",
  "",
].join("\n")

test("file mode: user role can write but not run admin commands", async () => {
  await withClient(
    { authFile: USERS_FILE },
    { auth: { user: "bob", password: "hunter2" } },
    async (db) => {
      await db.put("k", { a: 1 })
      assert.equal(await db.incr("c"), 1)
      assert.deepEqual(await db.stats().then((s) => typeof s.records), "number")
      for (const call of [db.compact(), db.reindex(), db.relieve()]) {
        await assert.rejects(call, /permission denied: \w+ requires admin/)
      }
    },
  )
})

test("file mode: admin role runs admin commands; bad credentials reject", async () => {
  await withServer({ authFile: USERS_FILE }, async (srv) => {
    const admin = await NteeClient.connect({
      host: srv.host,
      port: srv.port,
      auth: { user: "root", password: "changeme" },
    })
    try {
      assert.equal(await admin.compact(), true)
      assert.equal(await admin.reindex(), true)
      assert.equal(await admin.relieve(), true)
    } finally {
      await admin.close()
    }
    await assert.rejects(
      NteeClient.connect({
        host: srv.host,
        port: srv.port,
        auth: { user: "eve", password: "x" },
      }),
      /invalid user or password/,
    )
  })
})

test("auth mode none: auth() is a harmless no-op", async () => {
  await withClient({}, {}, async (db) => {
    assert.equal(await db.auth("anything"), true)
    assert.equal(await db.ping(), "pong")
  })
})

test("server errors carry the failing command", async () => {
  await withClient({}, {}, async (db) => {
    const err = await db
      .incr("c")
      .then(() => db.put("c", "demote").then(() => db.incr("c")))
      .catch((e) => e)
    assert.ok(err instanceof NteeServerError)
    assert.equal(err.command, "incr")
  })
})

// Pure-JavaScript TCP/TLS client for nteedb-server. Zero runtime
// dependencies — node:net / node:tls plus the protocol helpers. One request
// line (or length-prefixed frame) per command, one JSON response line per
// command, in order; the client pipelines freely over a FIFO of pending
// resolvers.
import net from "node:net"
import tls from "node:tls"
import {
  toStorable,
  assertToken,
  decodeValue,
  buildPut,
  buildPutx,
  buildPutex,
  ttlArg,
} from "./protocol.js"

/** A command the server answered with {"ok":false,"err":…}. The connection
 * remains usable (except after protocol-fatal errors, where the server
 * closes the socket right after this rejection). */
export class NteeServerError extends Error {
  /** The command word that failed (e.g. "get", "putx"). */
  command
  constructor(message, command) {
    super(message)
    this.name = "NteeServerError"
    this.command = command
  }
}

/** The connection failed or was closed: socket error, server shutdown,
 * protocol desync, or a method call on a closed client. Every in-flight
 * command rejects with this; the client cannot be reused — create a new one. */
export class NteeConnectionError extends Error {
  constructor(message) {
    super(message)
    this.name = "NteeConnectionError"
  }
}

function assertSafeInt(name, v) {
  if (!Number.isSafeInteger(v)) {
    throw new TypeError(`nteedb: ${name} must be a safe integer`)
  }
}

// An index value may be a string token or a finite number (the server
// coerces the token to the index's declared kind).
function indexValueToken(name, val) {
  if (typeof val === "number") {
    if (!Number.isFinite(val)) {
      throw new TypeError(`nteedb: ${name} must be finite`)
    }
    return String(val)
  }
  assertToken(name, val)
  return val
}

export class NteeClient {
  #socket
  #pending = [] // FIFO of {command, resolve, reject, decode}
  #recv = Buffer.alloc(0)
  #closed = false
  #closeError = null

  /** Server info from the hello handshake at connect (or the latest hello()/
   * auth()): {server, version, auth, indexes?}. indexes is present only once
   * the connection is authenticated. */
  info = null

  constructor(socket) {
    this.#socket = socket
    socket.setNoDelay(true)
    socket.on("data", (chunk) => this.#onData(chunk))
    socket.on("error", (err) => {
      this.#closeError ??= new NteeConnectionError(
        `nteedb: connection error: ${err.message}`,
      )
    })
    socket.on("close", () => {
      this.#closed = true
      const err =
        this.#closeError ?? new NteeConnectionError("nteedb: connection closed")
      const pending = this.#pending
      this.#pending = []
      for (const p of pending) p.reject(err)
    })
  }

  /**
   * Connect to a server and perform the handshake. If `auth` is given —
   * a string (shared-password mode) or {user, password} (auth-file mode) —
   * it is sent first; a final hello populates client.info (the server only
   * reveals its index schema to authenticated connections). With no `auth`
   * against an auth-requiring server, connect still succeeds (hello is
   * pre-auth); data commands fail with "auth required" until auth() is
   * called.
   *
   * TLS: pass `tls: true` (system CAs) or a node:tls options object
   * ({ca, rejectUnauthorized, servername, ...}) to connect to the server's
   * TLS listener. The port defaults to 6667 with TLS, 6666 without —
   * matching the server's default -tls-addr / -addr.
   */
  static async connect({
    host = "127.0.0.1",
    tls: tlsOpts = undefined,
    port = tlsOpts ? 6667 : 6666,
    auth = undefined,
    connectTimeout = 5000,
  } = {}) {
    const socket = await new Promise((resolve, reject) => {
      const s = tlsOpts
        ? tls.connect({
            host,
            port,
            ...(tlsOpts === true ? {} : tlsOpts),
          })
        : net.createConnection({ host, port })
      const timer = setTimeout(() => {
        s.destroy()
        reject(
          new NteeConnectionError(
            `nteedb: connect timeout after ${connectTimeout}ms (${host}:${port})`,
          ),
        )
      }, connectTimeout)
      // A TLS socket is ready only after the handshake completes.
      s.once(tlsOpts ? "secureConnect" : "connect", () => {
        clearTimeout(timer)
        resolve(s)
      })
      s.once("error", (err) => {
        clearTimeout(timer)
        reject(
          new NteeConnectionError(`nteedb: connect failed: ${err.message}`),
        )
      })
    })
    const client = new NteeClient(socket)
    try {
      if (auth !== undefined) {
        await client.auth(auth) // also refreshes info via hello
      } else {
        await client.hello()
      }
    } catch (err) {
      socket.destroy()
      throw err
    }
    return client
  }

  /** Whether the connection is closed (no further commands possible). */
  get closed() {
    return this.#closed
  }

  #onData(chunk) {
    this.#recv = this.#recv.length ? Buffer.concat([this.#recv, chunk]) : chunk
    let nl
    while ((nl = this.#recv.indexOf(10)) >= 0) {
      const line = this.#recv.subarray(0, nl).toString()
      this.#recv = this.#recv.subarray(nl + 1)
      let env
      try {
        env = JSON.parse(line)
      } catch {
        this.#fatal(`nteedb: malformed response line: ${line.slice(0, 120)}`)
        return
      }
      const p = this.#pending.shift()
      if (!p) {
        this.#fatal("nteedb: response with no command in flight (desync)")
        return
      }
      if (env.ok === false) {
        p.reject(new NteeServerError(env.err ?? "unknown error", p.command))
      } else {
        try {
          p.resolve(p.decode ? p.decode(env) : env.result)
        } catch (err) {
          p.reject(err)
        }
      }
    }
  }

  #fatal(message) {
    this.#closeError ??= new NteeConnectionError(message)
    this.#socket.destroy() // 'close' rejects all pending
  }

  /** Write one contiguous frame and register its response slot. */
  #send(command, frame, decode) {
    if (this.#closed) {
      return Promise.reject(new NteeConnectionError("nteedb: client is closed"))
    }
    return new Promise((resolve, reject) => {
      this.#pending.push({ command, resolve, reject, decode })
      this.#socket.write(frame)
    })
  }

  #cmd(command, args = [], decode) {
    const line =
      args.length === 0 ? `${command}\r\n` : `${command} ${args.join(" ")}\r\n`
    return this.#send(command, Buffer.from(line), decode)
  }

  // ---- session ----

  /** Re-run the hello handshake: {server, version, auth, indexes?}.
   * indexes appears only once authenticated. Updates client.info. */
  async hello() {
    const info = await this.#cmd("hello")
    this.info = info
    return info
  }

  /**
   * Authenticate: auth("password") for shared-password mode, or
   * auth({user, password}) for auth-file mode. Resolves true and refreshes
   * client.info (hello now includes the index schema). In auth mode "none"
   * this is a harmless no-op.
   */
  async auth(credentials) {
    let args
    if (typeof credentials === "string") {
      assertToken("password", credentials)
      args = [credentials]
    } else if (credentials !== null && typeof credentials === "object") {
      assertToken("user", credentials.user)
      assertToken("password", credentials.password)
      args = [credentials.user, credentials.password]
    } else {
      throw new TypeError(
        "nteedb: auth expects a password string or {user, password}",
      )
    }
    const res = await this.#cmd("auth", args)
    await this.hello()
    return res
  }

  /** Liveness check; resolves "pong". Requires auth (like every data
   * command) — also useful to keep an idle connection inside the server's
   * idle timeout (5 minutes by default). */
  ping() {
    return this.#cmd("ping")
  }

  /** Graceful disconnect: sends quit, awaits the acknowledgment and the
   * server-side close. Idempotent. */
  async quit() {
    if (this.#closed) return
    const closed = new Promise((resolve) => {
      this.#socket.once("close", resolve)
      setTimeout(() => {
        this.#socket.destroy()
        resolve()
      }, 1000).unref()
    })
    try {
      await this.#cmd("quit")
    } catch {
      // The server may close before the response is read; that's still a
      // successful quit.
    }
    await closed
  }

  /** Alias for quit(). */
  close() {
    return this.quit()
  }

  // ---- reads ----

  /** Get the stored value for key, or null when absent. Valid-JSON values
   * arrive parsed; any other bytes (plain text, binary — and counters,
   * whose fixed-width form is not JSON) arrive as a Buffer. Read counters
   * with incr(key, 0) instead. */
  get(key) {
    assertToken("key", key)
    return this.#cmd("get", [key], (env) =>
      env.found ? decodeValue(env.result) : null,
    )
  }

  /** Get many values in one round trip, aligned to keys: each entry is the
   * decoded value or null when that key is absent. */
  getMany(keys) {
    if (!Array.isArray(keys) || keys.length === 0) {
      throw new TypeError("nteedb: getMany expects a non-empty array of keys")
    }
    for (const k of keys) assertToken("key", k)
    return this.#cmd("getm", keys, (env) =>
      env.result.map((row) => (row.found ? decodeValue(row.value) : null)),
    )
  }

  /** Whether key exists. */
  has(key) {
    assertToken("key", key)
    return this.#cmd("has", [key])
  }

  /** Sorted primary keys with the given prefix ("" or omitted = all keys). */
  prefixScan(prefix = "") {
    if (prefix === "") return this.#cmd("scan")
    assertToken("prefix", prefix)
    return this.#cmd("scan", [prefix])
  }

  /** Primary keys whose index value equals val. limit: 0 = all ascending,
   * N>0 = first N ascending, N<0 = last |N| descending. */
  secIndex(name, val, limit = 0) {
    assertToken("index", name)
    assertSafeInt("limit", limit)
    const args = [name, indexValueToken("value", val)]
    if (limit !== 0) args.push(String(limit))
    return this.#cmd("ix", args)
  }

  /** Whether any record carries val in the index. */
  secIndexHas(name, val) {
    assertToken("index", name)
    return this.#cmd("ixh", [name, indexValueToken("value", val)])
  }

  /** Keys whose string index value starts with prefix. The limit applies
   * per distinct index value: N>0 = first N of each value, N<0 = last |N|
   * of each value (descending). */
  secIndexPrefix(name, prefix, limit = 0) {
    assertToken("index", name)
    assertToken("prefix", prefix)
    assertSafeInt("limit", limit)
    const args = [name, prefix]
    if (limit !== 0) args.push(String(limit))
    return this.#cmd("ixp", args)
  }

  /** Keys whose index value is in [lo, hi], inclusive on both ends. */
  secIndexRange(name, lo, hi) {
    assertToken("index", name)
    return this.#cmd("ixr", [
      name,
      indexValueToken("lo", lo),
      indexValueToken("hi", hi),
    ])
  }

  /** Like secIndex but returns [{key, value}] rows with decoded values.
   * Keys deleted between the index read and the value fetch are dropped by
   * the server, so rows may be fewer than secIndex would return. */
  secIndexRecords(name, val, limit = 0) {
    assertToken("index", name)
    assertSafeInt("limit", limit)
    const args = [name, indexValueToken("value", val)]
    if (limit !== 0) args.push(String(limit))
    return this.#cmd("ixrec", args, (env) =>
      env.result.map((row) => ({
        key: row.key,
        value: decodeValue(row.value),
      })),
    )
  }

  // ---- writes ----

  /**
   * Store value under key. value: string | Buffer | any JSON-serializable
   * (objects are stringified; undefined throws). With ix — a plain object of
   * {indexName: string|number} — the write is indexed (server-side putx);
   * indexed values must be JSON objects. Always uses the length-prefixed
   * wire form, so values may be empty, binary, or contain newlines.
   *
   * ttlMs (optional, positive integer) gives the key a time-to-live: once
   * it elapses the key reads as missing and is lazily deleted server-side.
   * A put WITHOUT a ttl clears any existing TTL. Secondary indexes are
   * TTL-unaware — an expired key may appear in secIndex* key results until
   * cleanup (record fetches already drop it).
   */
  put(key, value, ix = undefined, ttlMs = undefined) {
    assertToken("key", key)
    const ttl = ttlArg(ttlMs)
    const v = toStorable(value)
    const valueBuf = Buffer.isBuffer(v) ? v : Buffer.from(v)
    if (ix === undefined) {
      return ttl > 0
        ? this.#send("putex", buildPutex(key, ttl, valueBuf))
        : this.#send("put", buildPut(key, valueBuf))
    }
    if (ix === null || typeof ix !== "object" || Array.isArray(ix)) {
      throw new TypeError(
        "nteedb: ix must be a plain object of {indexName: value}",
      )
    }
    return this.#send(
      "putx",
      buildPutx(key, Buffer.from(JSON.stringify(ix)), valueBuf, ttl),
    )
  }

  /** Delete key. Resolves true whether or not the key existed. */
  delete(key) {
    assertToken("key", key)
    return this.#cmd("del", [key])
  }

  /** Atomically add delta (default 1) to the int64 counter at key; resolves
   * to the new value. A missing key initializes to 0 first — incr(key, 0)
   * reads a counter. Rejects on non-counter values and int64 overflow.
   * Values beyond ±2^53 lose precision as JS numbers.
   *
   * ttlMs (optional) applies ONLY when this call creates the key — a live
   * counter keeps its deadline, an expired one restarts from 0 with the new
   * ttl. incr(w, 1, 60000) is a complete fixed-window rate limiter. */
  incr(key, delta = 1, ttlMs = undefined) {
    assertToken("key", key)
    assertSafeInt("delta", delta)
    const ttl = ttlArg(ttlMs)
    const args = [key, String(delta)]
    if (ttl > 0) args.push(String(ttl))
    return this.#cmd("incr", args)
  }

  /** Atomically subtract delta (default 1); incr with a negated delta. */
  decr(key, delta = 1, ttlMs = undefined) {
    assertToken("key", key)
    assertSafeInt("delta", delta)
    const ttl = ttlArg(ttlMs)
    const args = [key, String(delta)]
    if (ttl > 0) args.push(String(ttl))
    return this.#cmd("decr", args)
  }

  /** Atomically add up to amount (>= 0) to the counter, clamped at max;
   * resolves to the overflow that did NOT fit (0 = fully applied; a counter
   * already at/above max is left unchanged). Missing key counts as 0 and is
   * created when anything is added. ttlMs applies on create only. */
  topup(key, amount, max, ttlMs = undefined) {
    assertToken("key", key)
    assertSafeInt("amount", amount)
    assertSafeInt("max", max)
    const ttl = ttlArg(ttlMs)
    const args = [key, String(amount), String(max)]
    if (ttl > 0) args.push(String(ttl))
    return this.#cmd("topup", args)
  }

  /** Atomically subtract amount (>= 0) only if the result stays >= left;
   * resolves true iff applied (false leaves the value untouched). ttlMs
   * applies on create only. */
  take(key, amount, left, ttlMs = undefined) {
    assertToken("key", key)
    assertSafeInt("amount", amount)
    assertSafeInt("left", left)
    const ttl = ttlArg(ttlMs)
    const args = [key, String(amount), String(left)]
    if (ttl > 0) args.push(String(ttl))
    return this.#cmd("take", args)
  }

  /** Delete every key strictly below cutoff; resolves to the count. */
  removeByPkLess(cutoff) {
    assertToken("cutoff", cutoff)
    return this.#cmd("rml", [cutoff])
  }

  /** Delete every key strictly above cutoff; resolves to the count. */
  removeByPkGreater(cutoff) {
    assertToken("cutoff", cutoff)
    return this.#cmd("rmg", [cutoff])
  }

  // ---- introspection ----

  /** Server/store statistics: {records, mainBytes, liveBytes, blobBytes,
   * connections, totalConns, commands, autoCompacts, blobCompacts}. */
  stats() {
    return this.#cmd("stats")
  }

  /** Index names dropped from the schema whose values still linger. */
  secIndexDropped() {
    return this.#cmd("dropped")
  }

  /** Declared index names not yet back-filled over existing records. */
  secIndexProspective() {
    return this.#cmd("prospective")
  }

  // ---- admin (requires the admin role; may take seconds — no timeout) ----

  /** Rewrite the main log dropping dead records. Admin only. */
  compact() {
    return this.#cmd("compact")
  }

  /** Back-fill derived indexes over history, purge dropped ones. Admin only. */
  reindex() {
    return this.#cmd("reindex")
  }

  /** Unconditional blob rewrite + compaction. Admin only. */
  relieve() {
    return this.#cmd("relieve")
  }

  // ---- escape hatch ----

  /**
   * Send a raw command line verbatim and resolve with the parsed envelope
   * {ok, found?, result?, err?} — no throw on ok:false, no value decoding.
   * The line must not contain newlines and must not be blank (the server
   * sends no response for blank lines). Data-block commands (put/putx) are
   * not expressible here — use put().
   */
  command(line) {
    if (typeof line !== "string" || line.trim().length === 0) {
      throw new TypeError("nteedb: command line must be a non-empty string")
    }
    if (/[\r\n]/.test(line)) {
      throw new TypeError("nteedb: command line must not contain newlines")
    }
    const word = line.trim().split(/\s+/)[0].toLowerCase()
    return this.#send(word, Buffer.from(line + "\r\n"), (env) => env).catch(
      (err) => {
        if (err instanceof NteeServerError) {
          return { ok: false, err: err.message }
        }
        throw err
      },
    )
  }
}

export default NteeClient

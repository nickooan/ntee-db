/// <reference types="node" />

/** A value read back from the store: valid-JSON bytes arrive parsed (object,
 * array, string, number, boolean, null); any other bytes — plain text,
 * binary, counters — arrive as a Buffer. Narrow or cast at the call site. */
export type ReadValue = unknown

/** A value accepted for storage: strings and Buffers are stored verbatim;
 * anything else is JSON-serialized. undefined throws. */
export type Value =
  string | Buffer | object | number | boolean | null | unknown[]

/** Secondary-index values for an indexed put. */
export type IndexValues = { [index: string]: string | number }

/** Credentials for connect()/auth(): a shared password (server -auth mode)
 * or a {user, password} pair (server -auth-file mode). */
export type AuthCredentials = string | { user: string; password: string }

export interface NteeClientOptions {
  /** Server host. Default "127.0.0.1". */
  host?: string
  /** Server port. Default 6740 (the server's default -addr port). */
  port?: number
  /** Authenticate during connect. Omit against auth-mode "none" servers, or
   * to connect unauthenticated and call auth() later (only hello/auth/quit
   * work before authentication). */
  auth?: AuthCredentials
  /** Milliseconds allowed for the TCP dial + handshake. Default 5000.
   * Commands themselves have no client-side timeout (admin commands may
   * legitimately take seconds). */
  connectTimeout?: number
}

/** Result of the hello handshake. */
export interface HelloInfo {
  server: string
  version: string
  /** The server's auth mode: "none" | "password" | "file". */
  auth: string
  /** Declared secondary indexes — present only once authenticated. */
  indexes?: { name: string; kind: "string" | "number" }[]
}

/** Result of stats(). Byte figures include dead records / orphaned blobs
 * until compaction. Counters are int64 server-side; values beyond 2^53 lose
 * precision as JS numbers. */
export interface Stats {
  records: number
  mainBytes: number
  liveBytes: number
  blobBytes: number
  connections: number
  totalConns: number
  commands: number
  autoCompacts: number
  blobCompacts: number
}

/** The raw response envelope, as returned by command(). */
export interface Envelope {
  ok: boolean
  /** Present only on get responses. */
  found?: boolean
  result?: unknown
  err?: string
}

/** A command the server rejected ({"ok":false}); the connection stays
 * usable except after protocol-fatal errors (oversized line/value), where
 * the server closes the socket immediately after this rejection. */
export declare class NteeServerError extends Error {
  /** The command word that failed (e.g. "get", "putx"). */
  command: string
}

/** The connection failed or was closed — socket error, server shutdown,
 * protocol desync, or use after close(). All in-flight commands reject with
 * this. The client cannot be reused; create a new one with connect(). */
export declare class NteeConnectionError extends Error {}

export declare class NteeClient {
  private constructor(socket: unknown)

  /**
   * Connect to a nteedb-server and perform the handshake. If options.auth is
   * given it is sent first; a hello then populates client.info (the index
   * schema is only revealed to authenticated connections). Without auth
   * against an auth-requiring server, connect still succeeds — data commands
   * fail with "auth required" until auth() is called.
   */
  static connect(options?: NteeClientOptions): Promise<NteeClient>

  /** Server info captured by the latest hello (at connect, hello(), or
   * auth()). */
  info: HelloInfo | null

  /** Whether the connection is closed. Every method on a closed client
   * rejects with NteeConnectionError. */
  readonly closed: boolean

  // -- session --

  /** Re-run the hello handshake and update client.info. */
  hello(): Promise<HelloInfo>
  /** Authenticate (password string or {user, password}); resolves true and
   * refreshes client.info. No-op in auth mode "none". */
  auth(credentials: AuthCredentials): Promise<true>
  /** Liveness check; resolves "pong". Requires auth. Also useful to keep an
   * idle connection inside the server's idle timeout (default 5 minutes —
   * the server closes silently when it expires). */
  ping(): Promise<string>
  /** Graceful disconnect (sends quit, awaits the server-side close).
   * Idempotent. */
  quit(): Promise<void>
  /** Alias for quit(). */
  close(): Promise<void>

  // -- reads --

  /** Value for key, or null when absent. Counters arrive as a Buffer (their
   * fixed-width form is not JSON) — read counters with incr(key, 0). */
  get(key: string): Promise<ReadValue | null>
  /** Values for many keys in one round trip, aligned to keys (null for
   * missing entries). */
  getMany(keys: string[]): Promise<(ReadValue | null)[]>
  /** Whether key exists. */
  has(key: string): Promise<boolean>
  /** Sorted primary keys with the given prefix ("" = all keys). */
  prefixScan(prefix?: string): Promise<string[]>
  /** Keys whose index value equals val. limit: 0 = all ascending, N>0 =
   * first N ascending, N<0 = last |N| descending. */
  secIndex(
    name: string,
    val: string | number,
    limit?: number,
  ): Promise<string[]>
  /** Whether any record carries val in the index. */
  secIndexHas(name: string, val: string | number): Promise<boolean>
  /** Keys whose string index value starts with prefix; the limit applies per
   * distinct index value. */
  secIndexPrefix(
    name: string,
    prefix: string,
    limit?: number,
  ): Promise<string[]>
  /** Keys whose index value lies in [lo, hi], inclusive. */
  secIndexRange(
    name: string,
    lo: string | number,
    hi: string | number,
  ): Promise<string[]>
  /** Like secIndex but returns {key, value} rows with decoded values (keys
   * deleted mid-query are dropped by the server). */
  secIndexRecords(
    name: string,
    val: string | number,
    limit?: number,
  ): Promise<{ key: string; value: ReadValue }[]>

  // -- writes --

  /** Store value under key; with ix, an indexed write (the value must then
   * be a JSON object). Always length-prefixed on the wire — values may be
   * empty, binary, or contain newlines. */
  put(key: string, value: Value, ix?: IndexValues): Promise<true>
  /** Delete key; resolves true whether or not it existed. */
  delete(key: string): Promise<true>
  /** Atomically add delta (default 1) to the int64 counter at key; resolves
   * to the new value. Missing key initializes to 0 — incr(key, 0) reads a
   * counter. Rejects on non-counter values and int64 overflow. */
  incr(key: string, delta?: number): Promise<number>
  /** Atomically subtract delta (default 1); incr with a negated delta. */
  decr(key: string, delta?: number): Promise<number>
  /** Atomically add up to amount (>= 0), clamped at max; resolves to the
   * overflow that did NOT fit (0 = fully applied; a counter already at or
   * above max is left unchanged). */
  topup(key: string, amount: number, max: number): Promise<number>
  /** Atomically subtract amount (>= 0) only if the result stays >= left;
   * resolves true iff applied. */
  take(key: string, amount: number, left: number): Promise<boolean>
  /** Delete every key strictly below cutoff; resolves to the count. */
  removeByPkLess(cutoff: string): Promise<number>
  /** Delete every key strictly above cutoff; resolves to the count. */
  removeByPkGreater(cutoff: string): Promise<number>

  // -- introspection --

  stats(): Promise<Stats>
  /** Dropped index names whose values still linger in records. */
  secIndexDropped(): Promise<string[]>
  /** Declared index names not yet back-filled (run reindex()). */
  secIndexProspective(): Promise<string[]>

  // -- admin (requires the admin role; can take seconds) --

  compact(): Promise<true>
  reindex(): Promise<true>
  relieve(): Promise<true>

  // -- escape hatch --

  /** Send a raw command line verbatim; resolves with the parsed envelope
   * (no throw on ok:false, no value decoding). Not for data-block commands
   * (put/putx) — use put(). */
  command(line: string): Promise<Envelope>
}

export default NteeClient

// Ergonomic Node.js wrapper around the nteedb C-shared library.
import { fns, readEnvelope, callAsync } from "./native.js"

// Normalize a put value: a Buffer or string passes through unchanged (raw bytes
// / verbatim text — no double-encoding), and anything else (object, array,
// number, boolean, null) is JSON-serialized. Lets callers store objects
// directly without a manual JSON.stringify.
function toStorable(value) {
  if (Buffer.isBuffer(value) || typeof value === "string") return value
  if (value === undefined) {
    // JSON.stringify(undefined) is undefined, which would surface later as a
    // cryptic Buffer.from error (put) or silently store empty (putMany).
    throw new TypeError(
      "nteedb: value is undefined (store null explicitly if intended)",
    )
  }
  return JSON.stringify(value)
}

// Decode one element of the inline-JSON read envelope: a parsed JSON value
// (already an object/array/scalar from the single envelope parse), a Buffer for
// a binary/non-JSON value ("v"), or null when the key is absent. A found value
// with neither field is an empty (non-JSON) value → an empty Buffer.
function decodeJson(rec) {
  if (!rec || !rec.found) return null
  if (rec.json !== undefined) return rec.json
  if (rec.v !== undefined) return Buffer.from(rec.v, "base64")
  return Buffer.alloc(0)
}

// Map the records read envelope (each row is a key plus the decodeJson value
// fields) to { key, value } records. Absent keys yield a null value.
function toRecords(recs) {
  return (recs ?? []).map((rec) => ({ key: rec.key, value: decodeJson(rec) }))
}

/**
 * NteeDB is an open handle to a store. Construct via NteeDB.open().
 */
export class NteeDB {
  #h
  #closed = false

  constructor(handle) {
    this.#h = handle
  }

  /**
   * Open (creating if needed) a store at `dir`.
   * opts: { blobThreshold?, syncEveryWrite?, hintEveryN?,
   *         indexes?: [{name, kind:'string'|'number', jsonPath?, maxPerValue?}] }
   * ntee-db is a JSON store: value-returning reads return the value parsed (a
   * binary/non-JSON value comes back as a Buffer).
   */
  static open(dir, opts = {}) {
    const handle = readEnvelope(fns.open(dir, JSON.stringify({ ...opts, dir })))
    return new NteeDB(handle)
  }

  /** Delete every file of the store at `dir` (no DB need be open). */
  static destroy(dir) {
    readEnvelope(fns.destroy(dir))
  }

  #assertOpen() {
    if (this.#closed) throw new Error("nteedb: database is closed")
  }

  /**
   * Store a value under key, with optional secondary index values. An object,
   * array, or scalar is JSON-serialized automatically; a string or Buffer is
   * stored as-is (Buffer for raw/binary content).
   */
  put(key, value, ix) {
    this.#assertOpen()
    const v = toStorable(value)
    const buf = Buffer.isBuffer(v) ? v : Buffer.from(v)
    const ixJSON = ix ? JSON.stringify(ix) : ""
    readEnvelope(fns.put(this.#h, key, buf, buf.length, ixJSON))
  }

  /**
   * Atomically add `delta` (default 1) to the int64 counter at `key` and
   * resolve to the new value. A missing key initializes to 0 before the delta
   * applies, so `incr(key, 0)` reads a counter (creating it at 0 if absent).
   * Counters are a distinct value type: incr on a key holding any other value
   * rejects ("non-counter value"), a put() on a counter key demotes it to a
   * plain value, and overflowing int64 rejects leaving the value unchanged.
   * Counters never participate in secondary indexes (primary-key-only), and
   * increments update the record in place — a hot counter never grows the log.
   * Deltas must be safe integers (|delta| ≤ 2^53-1); counter values beyond
   * that range lose precision as JS numbers.
   */
  incr(key, delta = 1) {
    this.#assertOpen()
    if (!Number.isSafeInteger(delta))
      throw new TypeError("nteedb: delta must be a safe integer")
    return callAsync(fns.incr, this.#h, key, delta)
  }

  /**
   * Atomically subtract `delta` (default 1) from the counter at `key` and
   * resolve to the new value — incr() with a negated delta; see incr() for
   * counter semantics.
   */
  decr(key, delta = 1) {
    this.#assertOpen()
    if (!Number.isSafeInteger(delta))
      throw new TypeError("nteedb: delta must be a safe integer")
    return callAsync(fns.incr, this.#h, key, -delta)
  }

  /**
   * Append many records in one batch — the bulk counterpart to put() for
   * imports and other high-volume writes: one FFI crossing, one lock, one
   * fsync in durable mode. Items are applied in array order (a repeated key's
   * later item wins); an invalid item rejects the whole batch with nothing
   * written. Runs off the event loop; the returned promise resolves to the
   * number of records once ALL of them are appended — synchronous durability
   * at the batch boundary, unlike lmdb-style fire-and-forget batching.
   *
   * items: [{ key, value: object|string|Buffer, ix? }] — a value is JSON-
   * serialized unless it is already a string or Buffer (as in put()).
   *
   * Wire format: only keys, value byte-lengths, and index values travel as JSON;
   * the value bytes ride a single concatenated buffer (no per-value escaping or
   * base64). Runs off the event loop.
   */
  putMany(items) {
    this.#assertOpen()
    const metas = new Array(items.length)
    const bufs = new Array(items.length)
    for (let i = 0; i < items.length; i++) {
      const { key, value, ix } = items[i]
      const val = toStorable(value)
      const buf = Buffer.isBuffer(val) ? val : Buffer.from(val, "utf8")
      bufs[i] = buf
      metas[i] = ix ? { k: key, n: buf.length, ix } : { k: key, n: buf.length }
    }
    const blob = Buffer.concat(bufs)
    return callAsync(
      fns.putBatchBin,
      this.#h,
      JSON.stringify(metas),
      blob,
      blob.length,
    )
  }

  /**
   * Get the value for key, parsed, or null if absent. Values are returned via
   * JSON parse semantics (a stored `"123"` reads back as `123`); a binary or
   * non-JSON value comes back as a Buffer.
   *
   * Async: the native read runs off the event loop (a libuv worker), so the JS
   * thread stays free and concurrent reads run in parallel. Resolves to the value.
   */
  get(key) {
    this.#assertOpen()
    return callAsync(fns.getJson, this.#h, key).then(decodeJson)
  }

  /**
   * Get the values for many keys in one FFI call, aligned to `keys`: each entry
   * is the parsed value (or a Buffer for binary/non-JSON), or null if the key is
   * absent. The batched counterpart to get() — used by the *Records searches so
   * fetching N values is one crossing.
   *
   * Async: the result set is unbounded, so the native read runs off the event
   * loop (a libuv worker) rather than blocking the JS thread. Resolves to the
   * aligned value array. (The envelope JSON.parse still runs on-loop.)
   */
  getMany(keys) {
    this.#assertOpen()
    return callAsync(fns.getManyJson, this.#h, JSON.stringify(keys)).then(
      (res) => (res ?? []).map(decodeJson),
    )
  }

  /** Whether key exists. Async — runs off the event loop; resolves to a boolean. */
  has(key) {
    this.#assertOpen()
    return callAsync(fns.has, this.#h, key).then((r) => r === true)
  }

  /**
   * Point-in-time store size: { records, mainBytes, blobBytes }. Cheap — all
   * values are in-memory counters (no I/O). mainBytes/blobBytes include dead
   * records / orphaned blobs until compact(). Async for a uniform read surface;
   * resolves to the stats object.
   */
  stats() {
    this.#assertOpen()
    return callAsync(fns.stats, this.#h)
  }

  /** Delete key (no-op if absent). */
  delete(key) {
    this.#assertOpen()
    readEnvelope(fns.delete(this.#h, key))
  }

  /**
   * Sorted keys with the given primary-key prefix. Async — the traversal runs
   * off the event loop (a libuv worker), so concurrent scans (e.g. via
   * Promise.all) run in parallel. Resolves to the key array.
   */
  prefixScan(prefix) {
    this.#assertOpen()
    return callAsync(fns.prefixScan, this.#h, prefix).then((r) => r ?? [])
  }

  /**
   * Primary keys whose value in the secondary index `name` equals `val`
   * (multi-value).
   * limit: 0 = all (ascending); N>0 = first N ascending; N<0 = last |N| descending.
   * Async — runs off the event loop; resolves to the key array.
   */
  secIndex(name, val, limit = 0) {
    this.#assertOpen()
    return callAsync(
      fns.byIndex,
      this.#h,
      name,
      JSON.stringify(val),
      limit,
    ).then((r) => r ?? [])
  }

  /**
   * Whether any record has `val` in the secondary index `name` — the
   * secondary-index counterpart of has(), without materializing the keys.
   * Async — runs off the event loop; resolves to a boolean.
   */
  secIndexHas(name, val) {
    this.#assertOpen()
    return callAsync(fns.byIndexHas, this.#h, name, JSON.stringify(val)).then(
      (r) => r === true,
    )
  }

  /**
   * Primary keys whose (string) value in the secondary index `name` starts with
   * `prefix`.
   * limit is applied per distinct index value (grouped): 0 = all matches flat;
   * N>0 = first N of each value ascending; N<0 = last |N| of each value descending.
   * Async — runs off the event loop; resolves to the key array.
   */
  secIndexPrefix(name, prefix, limit = 0) {
    this.#assertOpen()
    return callAsync(fns.byIndexPrefix, this.#h, name, prefix, limit).then(
      (r) => r ?? [],
    )
  }

  /**
   * Primary keys whose value in the secondary index `name` is within [lo, hi].
   * Async — runs off the event loop; resolves to the key array.
   */
  secIndexRange(name, lo, hi) {
    this.#assertOpen()
    return callAsync(
      fns.byIndexRange,
      this.#h,
      name,
      JSON.stringify(lo),
      JSON.stringify(hi),
    ).then((r) => r ?? [])
  }

  /** Indexes lingering in records after a soft-drop (until Reindex). Async. */
  secIndexDropped() {
    this.#assertOpen()
    return callAsync(fns.droppedIndexes, this.#h).then((r) => r ?? [])
  }

  /** Indexes not yet back-filled over pre-existing records. Async. */
  secIndexProspective() {
    this.#assertOpen()
    return callAsync(fns.prospectiveIndexes, this.#h).then((r) => r ?? [])
  }

  /**
   * Delete every key strictly less than `cutoff` (the cutoff key itself is
   * kept). Keys are compared lexically, so the caller's key design decides what
   * the range means. Async — runs off the event loop; resolves to the number of
   * keys removed. Does not reclaim disk (call compact() for that).
   */
  removeByPkLess(cutoff) {
    this.#assertOpen()
    return callAsync(fns.removeByPkLess, this.#h, cutoff)
  }

  /**
   * Delete every key strictly greater than `cutoff` (the cutoff key itself is
   * kept). Async; resolves to the number of keys removed.
   */
  removeByPkGreater(cutoff) {
    this.#assertOpen()
    return callAsync(fns.removeByPkGreater, this.#h, cutoff)
  }

  /** Reclaim dead records (async — runs off the event loop). */
  compact() {
    this.#assertOpen()
    return callAsync(fns.compact, this.#h)
  }

  /** Back-fill Extract-based indexes over history + purge dropped (async). */
  reindex() {
    this.#assertOpen()
    return callAsync(fns.reindex, this.#h)
  }

  /** Flush and close. */
  close() {
    if (this.#closed) return
    this.#closed = true
    readEnvelope(fns.close(this.#h))
  }

  /** Close and delete all of the store's files. */
  drop() {
    if (this.#closed) throw new Error("nteedb: database is closed")
    this.#closed = true
    readEnvelope(fns.drop(this.#h))
  }

  /**
   * Search a secondary index and return records {key, value} in one call. value
   * is the parsed JSON (a Buffer for binary/non-JSON), like get().
   * limit: 0 = all; N>0 = first N ascending; N<0 = last |N| descending.
   *
   * Async: the index walk + record fetch happen in a single native call off the
   * event loop (one FFI crossing, not a keys query followed by getMany).
   */
  secIndexRecords(name, val, limit = 0) {
    this.#assertOpen()
    return callAsync(
      fns.byIndexRecordsJson,
      this.#h,
      name,
      JSON.stringify(val),
      limit,
    ).then(toRecords)
  }

  /**
   * Search by secondary-index prefix (string index) and return records
   * {key, value} (value parsed as in get()). limit as secIndexPrefix (grouped
   * per distinct value); records come back ordered by (index value, then
   * selected primary key). One native call, off the event loop.
   */
  secIndexPrefixRecords(name, prefix, limit = 0) {
    this.#assertOpen()
    return callAsync(
      fns.byIndexPrefixRecordsJson,
      this.#h,
      name,
      prefix,
      limit,
    ).then(toRecords)
  }

  /**
   * Search by primary-key prefix, returning records {key, value} (value parsed
   * as in get()). One native call, off the event loop.
   */
  prefixScanRecords(prefix) {
    this.#assertOpen()
    return callAsync(fns.prefixScanRecordsJson, this.#h, prefix).then(toRecords)
  }
}

export default NteeDB

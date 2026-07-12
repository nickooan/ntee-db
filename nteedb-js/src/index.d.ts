export type ValueKind = "string" | "number"

export interface IndexDef {
  /** Index name (used in secIndex queries and as the key in put's ix values). */
  name: string
  /** Value type stored in the index. */
  kind: ValueKind
  /**
   * Optional dotted JSON path; when set, the index value is derived
   * automatically from each record's JSON value (the "scan itself" mode), and
   * reindex() can back-fill it over historical records. Objects only — the
   * path cannot traverse arrays.
   */
  jsonPath?: string
  /**
   * Cap on records sharing one value in this index. A write that pushes a
   * value's group over the cap evicts the oldest record(s) — lowest primary
   * key in the group — as a full, durable delete (the record leaves the store
   * and every index; design time-ordered keys so lowest pk = oldest).
   * 0 or omitted = unlimited.
   */
  maxPerValue?: number
}

export interface OpenOptions {
  /** Values >= this many bytes are stored in the blob side file. 0 = 64 KiB default; negative disables blobs. */
  blobThreshold?: number
  /**
   * fsync every write (power-loss durable; each write pays ~ms). When false
   * (default), writes still reach the OS immediately (no in-process buffer):
   * a process crash loses nothing; only power loss can drop recent writes.
   */
  syncEveryWrite?: boolean
  /** Rewrite the index hint after this many writes (also on close/compact). 0 disables periodic rewrites. */
  hintEveryN?: number
  /** Secondary indexes to maintain. */
  indexes?: IndexDef[]
}

/**
 * A value to store: a string or Buffer is stored as-is (Buffer = raw/binary);
 * an object, array, or scalar is JSON-serialized automatically.
 */
export type Value = Buffer | string | object | number | boolean | null

/**
 * A read value: the stored JSON parsed (object/array/scalar), or a `Buffer` for
 * a binary/non-JSON value. `unknown` because the store is value-agnostic on
 * write — narrow or cast at the call site.
 */
export type ReadValue = unknown

export interface Record {
  key: string
  value: ReadValue
}

/** Point-in-time store size. Sizes include dead records / orphaned blobs until compact(). */
export interface StoreStats {
  /** Live records (primary keys). */
  records: number
  /** main.jsonl bytes. */
  mainBytes: number
  /** blobs.dat bytes. */
  blobBytes: number
}

export declare class NteeDB {
  private constructor(handle: number)

  /** Open (creating if needed) a store at `dir`. */
  static open(dir: string, opts?: OpenOptions): NteeDB
  /** Delete every file of the store at `dir` (no DB need be open). */
  static destroy(dir: string): void

  /**
   * Store `value` under `key`, with optional secondary index values. Explicit
   * index values require the value to be a JSON object — immediate values
   * (strings, numbers, booleans, arrays, binary) are primary-key-only and
   * rejected with an error when `ix` is supplied.
   */
  put(
    key: string,
    value: Value,
    ix?: { [index: string]: string | number },
  ): void

  /**
   * Atomically add `delta` (default 1) to the int64 counter at `key`; resolves
   * to the new value. A missing key initializes to 0 first, so `incr(key, 0)`
   * reads a counter (creating it at 0 if absent). Rejects on a key holding a
   * non-counter value, and on int64 overflow (value left unchanged). Counters
   * are primary-key-only (never in secondary indexes) and update in place — a
   * hot counter never grows the log. `delta` must be a safe integer; counter
   * values beyond ±2^53 lose precision as JS numbers.
   */
  incr(key: string, delta?: number): Promise<number>
  /**
   * Atomically subtract `delta` (default 1) from the counter at `key`;
   * resolves to the new value. Same semantics as incr() with a negated delta.
   */
  decr(key: string, delta?: number): Promise<number>
  /**
   * Append many records in one batch (one FFI crossing, one lock, one fsync in
   * durable mode). Applied in array order; an invalid item rejects the whole
   * batch with nothing written. Runs off the event loop; resolves to the
   * number of records once all are appended.
   */
  putMany(
    items: {
      key: string
      value: Value
      ix?: { [index: string]: string | number }
    }[],
  ): Promise<number>

  /**
   * Get the value for `key`, or null if absent (parsed; see ReadValue). Async:
   * runs off the event loop, so concurrent reads run in parallel.
   */
  get(key: string): Promise<ReadValue | null>
  /**
   * Get the values for many keys in one FFI call, aligned to `keys`: each entry
   * is the parsed value (see ReadValue), or null if that key is absent. Async:
   * the unbounded read runs off the event loop.
   */
  getMany(keys: string[]): Promise<(ReadValue | null)[]>
  /** Whether `key` exists. Async — runs off the event loop. */
  has(key: string): Promise<boolean>
  /** Point-in-time store size (cheap, in-memory counters). Async for a uniform read surface. */
  stats(): Promise<StoreStats>
  /** Delete `key` (no-op if absent). */
  delete(key: string): void

  /**
   * Sorted keys with the given primary-key prefix. Async — the traversal runs
   * off the event loop, so concurrent scans (e.g. via Promise.all) run in parallel.
   */
  prefixScan(prefix: string): Promise<string[]>
  /**
   * Primary keys whose value in the secondary index `name` equals `val` (multi-value).
   * @param limit 0 = all (ascending); N>0 = first N ascending; N<0 = last |N| descending.
   * Async — runs off the event loop.
   */
  secIndex(
    name: string,
    val: string | number,
    limit?: number,
  ): Promise<string[]>
  /** Whether any record has `val` in the secondary index `name` (no keys materialized). Async. */
  secIndexHas(name: string, val: string | number): Promise<boolean>
  /**
   * Primary keys whose (string) value in the secondary index `name` starts with `prefix`.
   * @param limit applied per distinct index value (grouped): 0 (default) = all
   * matches flat; N>0 = first N of each value ascending; N<0 = last |N| of each
   * value descending.
   * Async — runs off the event loop.
   */
  secIndexPrefix(
    name: string,
    prefix: string,
    limit?: number,
  ): Promise<string[]>
  /** Primary keys whose value in the secondary index `name` is within [lo, hi]. Async. */
  secIndexRange(
    name: string,
    lo: string | number,
    hi: string | number,
  ): Promise<string[]>

  /** Indexes lingering in records after a soft-drop (until reindex). Async. */
  secIndexDropped(): Promise<string[]>
  /** Indexes not yet back-filled over pre-existing records. Async. */
  secIndexProspective(): Promise<string[]>

  /**
   * Delete every key strictly less than `cutoff` (cutoff kept). Lexical key
   * comparison; runs off the event loop. Resolves to the number removed. Does
   * not reclaim disk — call compact() for that.
   */
  removeByPkLess(cutoff: string): Promise<number>
  /**
   * Delete every key strictly greater than `cutoff` (cutoff kept). Runs off the
   * event loop; resolves to the number removed.
   */
  removeByPkGreater(cutoff: string): Promise<number>

  /** Reclaim dead records (runs off the event loop). */
  compact(): Promise<void>
  /** Back-fill Extract-based indexes over history + purge dropped (off the event loop). */
  reindex(): Promise<void>

  /** Flush and close. */
  close(): void
  /** Close and delete all of the store's files. */
  drop(): void

  /**
   * Search a secondary index, returning records {key, value} in one call (keys
   * query + one batched getMany). limit as secIndex. Async: the record fetch
   * runs off the event loop.
   */
  secIndexRecords(
    name: string,
    val: string | number,
    limit?: number,
  ): Promise<Record[]>
  /**
   * Search by secondary-index prefix (string index), returning records
   * {key, value} ordered by (index value, then primary key). limit as
   * secIndexPrefix (grouped). Async.
   */
  secIndexPrefixRecords(
    name: string,
    prefix: string,
    limit?: number,
  ): Promise<Record[]>
  /** Search by primary-key prefix, returning records {key, value}. Async. */
  prefixScanRecords(prefix: string): Promise<Record[]>
}

export default NteeDB
